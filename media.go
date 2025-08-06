package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/novartc/sipbench/arp"
	"github.com/novartc/sipbench/pkg/xdp"
	"github.com/novartc/sipbench/routing"
	"github.com/novartc/sipbench/rtp"
	"github.com/pion/sdp/v3"
)

type mediaInfo struct {
	addr string
	port int
}

func getAudioMediaFromMultipartSDP(raw []byte, boundary string) ([]mediaInfo, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)

	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}

		contentType := part.Header.Get("Content-Type")
		if contentType == "application/sdp" {
			got, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			return getAudioMediaFromSDP(got)
		}
		_ = part.Close()
	}
	return nil, errors.New("no SDP in body")
}

func getAudioMediaFromSDP(raw []byte) ([]mediaInfo, error) {
	var remoteSD sdp.SessionDescription
	if err := remoteSD.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("unable to parse SDP: %v", err)
	}

	var infos []mediaInfo
	var globalAddr string
	if remoteSD.ConnectionInformation != nil {
		globalAddr = remoteSD.ConnectionInformation.Address.Address
	}
	for _, md := range remoteSD.MediaDescriptions {
		if md.MediaName.Media == "audio" {
			info := mediaInfo{
				addr: globalAddr,
				port: md.MediaName.Port.Value,
			}
			if md.ConnectionInformation != nil {
				info.addr = md.ConnectionInformation.Address.Address
			}
			infos = append(infos, info)
		}
	}
	return infos, nil
}

type media struct {
	ctx        context.Context
	socket     *xdp.Socket
	dest       net.UDPAddr
	buf        []byte
	playing    atomic.Bool
	txPkts     uint64
	txLossPkts uint64
	txBytes    uint64
	done       chan struct{}
	mu         sync.Mutex
}

func (u *baseUa) newMedia(ctx context.Context, remote, local mediaInfo) (*media, error) {
	iface, gateway, _, err := routing.Route(net.ParseIP(remote.addr))
	if err != nil {
		return nil, fmt.Errorf("failed to query route: %v", err)
	}
	nextHop := remote.addr
	if gateway != nil {
		nextHop = gateway.String()
	}
	dstMac, ok := arp.Get(nextHop)
	if !ok {
		return nil, fmt.Errorf("MAC not found: %s", nextHop)
	}

	m := &media{
		ctx:    ctx,
		socket: u.xdpSocketsLB.Next(),
		dest:   net.UDPAddr{IP: net.ParseIP(remote.addr).To4(), Port: remote.port},
		buf:    make([]byte, 1600),
	}

	if err := pktSerialize(m.buf, dstMac[:], iface.HardwareAddr, remote, local); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *media) playback(pkts []rtp.Packet) error {
	if len(pkts) == 0 || m == nil {
		return nil
	}
	if m.playing.Swap(true) {
		close(m.done)
	}
	done := make(chan struct{})
	m.done = done

	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		ts := pkts[0].GetTs()
		for _, pkt := range pkts {
			time.Sleep(time.Duration(pkt.GetTs()-ts) * time.Microsecond)
			ts = pkt.GetTs()

			select {
			case <-m.ctx.Done():
				return
			case <-done:
				return
			default:
			}

			// TODO: media address with nat
			// TODO: rewrite SSRC
			payload := pkt.GetPayload()
			copy(m.buf[42:], payload)
			pktChecksum(m.buf, len(payload))

			if err := m.socket.SendMsg(m.buf[:42+len(payload)]); err != nil {
				slog.Warn("failed to send packet", "error", err)
				m.txLossPkts++
				continue
			}
			m.txPkts++
			m.txBytes += uint64(len(pkt.GetPayload()) + 8)
		}
	}()

	return nil
}

func (m *media) stop() error {
	if m == nil {
		return nil
	}
	if !m.playing.CompareAndSwap(true, false) {
		return nil
	}

	if m.done != nil {
		close(m.done)
	}

	return nil
}

func (m *media) Close() error {
	_ = m.stop()
	return nil
}

func pktSerialize(buf []byte, remoteMac, localMac []byte, remote, local mediaInfo) error {
	// Ethernet
	if len(remoteMac) != 6 || len(localMac) != 6 {
		return fmt.Errorf("bad MAC: %v, %v", remoteMac, localMac)
	}
	copy(buf, remoteMac)
	copy(buf[6:], localMac)
	buf[12] = 0x08

	// IPv4
	buf[14] = 0x45
	buf[20] = 0x40
	buf[22] = 0x40
	buf[23] = 0x11
	copy(buf[26:], net.ParseIP(local.addr).To4())
	copy(buf[30:], net.ParseIP(remote.addr).To4())

	// UDP
	binary.BigEndian.PutUint16(buf[34:], uint16(local.port))
	binary.BigEndian.PutUint16(buf[36:], uint16(remote.port))

	return nil
}

func pktChecksum(buf []byte, payload int) {
	// IPv4
	binary.BigEndian.PutUint16(buf[16:], uint16(payload+28))
	buf[24] = 0x00
	buf[25] = 0x00
	binary.BigEndian.PutUint16(buf[24:], gopacket.FoldChecksum(gopacket.ComputeChecksum(buf[14:34], 0)))

	// UDP
	binary.BigEndian.PutUint16(buf[38:], uint16(payload+8))
	buf[40] = 0x00
	buf[41] = 0x00
}
