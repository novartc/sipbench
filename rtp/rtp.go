package rtp

import (
	"path"
	"strconv"
)

type Packet struct {
	ts int64
	// include RTP header
	payload []byte
}

func (p *Packet) GetTs() int64 { return p.ts }

func (p *Packet) GetPayload() []byte { return p.payload }

func LoadRTP(name string) ([]Packet, error) {
	ext := path.Ext(name)
	switch ext {
	case ".pcm":
		return LoadPCM(name)
	case ".pcap":
		return LoadPcap(name)
	default:
		duration, err := strconv.Atoi(name)
		if err != nil {
			return nil, err
		}
		return WhiteNoise(duration), nil
	}
}
