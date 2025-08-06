package rtp

import (
	"encoding/binary"
	"github.com/novartc/sipbench/pkg/pcm"
	"io"
	"math/rand/v2"
	"os"
)

// LoadPCM PCM --> PCMA
func LoadPCM(name string) ([]Packet, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var pkts []Packet
	var sequence uint16
	var ts int64
	ssrc := rand.Uint32()
	timestamp := uint32(100)
	pcmData := make([]byte, 320)
	for {
		pkt := Packet{
			ts:      ts,
			payload: make([]byte, 172),
		}

		n, err := f.Read(pcmData)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if n != len(pcmData) {
			break
		}

		// rtp header
		sequence++
		timestamp += 160
		pkt.payload[0] = 0x80
		pkt.payload[1] = 8
		binary.BigEndian.PutUint16(pkt.payload[2:], sequence)
		binary.BigEndian.PutUint32(pkt.payload[4:], timestamp)
		binary.BigEndian.PutUint32(pkt.payload[8:], ssrc)

		// rtp payload
		// pcm to pcma
		if err := pcm.LinearToALaw(pcmData, pkt.payload[12:]); err != nil {
			return nil, err
		}

		ts += 20000
		pkts = append(pkts, pkt)
	}

	return pkts, nil
}
