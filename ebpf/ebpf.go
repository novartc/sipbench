package ebpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

//go:generate bpf2go -cc clang xdpsipbench src/xdp_sipbench.c -- -I ./src/

type Interface interface {
	BindInterfaces(ifindexs []int) error
	Update(ports ...uint16) error
	LookupAndDelete(ports ...uint16) ([]XdpSipbenchRtpInfo, error)
}

type XdpSipbenchRtpInfo xdpsipbenchRtpInfo

type Manager struct {
	objs  xdpsipbenchObjects
	links []link.Link
}

func New() (*Manager, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("failed to remove memlock rlimit: %v", err)
	}

	m := &Manager{}

	if err := loadXdpsipbenchObjects(&m.objs, nil); err != nil {
		return nil, fmt.Errorf("loading objects: %v", err)
	}

	return m, nil
}

func (m *Manager) BindInterfaces(ifindexs []int) error {
	for _, ifindex := range ifindexs {
		slog.Info("bind xdp to interface", slog.Int("ifindex", ifindex))
		l, err := link.AttachXDP(link.XDPOptions{
			Program:   m.objs.Sipbench,
			Interface: ifindex,
		})
		if err != nil {
			if !strings.HasSuffix(err.Error(), "operation not supported") {
				return fmt.Errorf("failed to attach XDP program[%d]: %v", ifindex, err)
			}

			// The driver supports XDPDriverMode, but some enabled features block its activation
			// In such cases, it can downgrade to XDPGenericMode
			l, err = link.AttachXDP(link.XDPOptions{
				Program:   m.objs.Sipbench,
				Interface: ifindex,
				Flags:     link.XDPGenericMode,
			})
			if err != nil {
				return fmt.Errorf("failed to attach XDP program[%d]: %v", ifindex, err)
			}
		}
		m.links = append(m.links, l)
	}

	return nil
}

func (m *Manager) Update(ports ...uint16) error {
	var (
		keys     = make([]uint64, len(ports))
		rtpInfos = make([]xdpsipbenchRtpInfo, len(ports))
	)
	for i, port := range ports {
		keys[i] = uint64(htons(port))
	}

	if _, err := m.objs.Rules.BatchUpdate(keys, rtpInfos, &ebpf.BatchOptions{ElemFlags: uint64(ebpf.UpdateAny)}); err != nil {
		return fmt.Errorf("failed to update rules: %v", err)
	}
	return nil
}

func (m *Manager) LookupAndDelete(ports ...uint16) ([]XdpSipbenchRtpInfo, error) {
	keys := make([]uint64, 0, len(ports))
	infos := make([]XdpSipbenchRtpInfo, len(ports))
	for i, port := range ports {
		key := uint64(htons(port))
		if err := m.objs.Rules.Lookup(key, &infos[i]); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			slog.Warn("failed to lookup rules", "error", err, "port", port)
		} else {
			keys = append(keys, key)
		}
	}

	if _, err := m.objs.Rules.BatchDelete(keys, nil); err != nil {
		return infos, err
	}
	return infos, nil
}

func (m *Manager) Close() error {
	if len(m.links) > 0 {
		for _, l := range m.links {
			_ = l.Close()
		}
	}
	return m.objs.Close()
}

func htons(i uint16) uint16 {
	return binary.BigEndian.Uint16((*(*[2]byte)(unsafe.Pointer(&i)))[:])
}
