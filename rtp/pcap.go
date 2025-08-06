package rtp

import (
	"fmt"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

func LoadPcap(name string) ([]Packet, error) {
	handle, err := pcap.OpenOffline(name)
	if err != nil {
		return nil, fmt.Errorf("unable to open pcap file: %v", err)
	}

	linkType := handle.LinkType()
	var offset int
	switch linkType {
	case layers.LinkTypeLinuxSLL:
		offset = 2
	case layers.LinkTypeLinuxSLL2:
		offset = 6
	case layers.LinkTypeEthernet:
		offset = 0
	default:
		return nil, fmt.Errorf("unsupported pcap type: %s", linkType.String())
	}
	pkts := make([]Packet, 0, 1024)
	source := gopacket.NewPacketSource(handle, linkType)
	for packet := range source.Packets() {
		// TODO: check rtp packet
		pkts = append(pkts, Packet{
			ts:      packet.Metadata().Timestamp.UnixMicro(),
			payload: packet.Data()[42+offset:],
		})
	}
	return pkts, nil
}
