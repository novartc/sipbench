package rtp

import (
	"encoding/binary"
	"math/rand/v2"

	"github.com/novartc/sipbench/pkg/pcm"
)

// WhiteNoise duration(seconds)
func WhiteNoise(duration int) []Packet {
	maxN := 28000
	minN := -28000

	var pkts []Packet
	var sequence uint16
	var ts int64
	ssrc := rand.Uint32()
	timestamp := uint32(100)
	pcmData := make([]byte, 320)
	for i := 0; i < duration*1000/20; i += 1 {
		pkt := Packet{
			ts:      ts,
			payload: make([]byte, 172),
		}

		for j := 0; j < 160; j++ {
			n := rand.IntN(maxN-minN) + minN
			pcmData[2*j] = byte(n)
			pcmData[2*j+1] = byte(n >> 8)
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
			return nil
		}

		ts += 20000
		pkts = append(pkts, pkt)
	}
	return pkts
}
