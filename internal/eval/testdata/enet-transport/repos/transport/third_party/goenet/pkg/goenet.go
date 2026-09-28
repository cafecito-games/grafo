package goenet

type PacketFlag uint32

const (
	PacketFlagReliable    PacketFlag = 1
	PacketFlagUnsequenced PacketFlag = 2
)

type Packet struct {
	Data  []byte
	Flags PacketFlag
}

type Peer struct{}

func (p *Peer) Send(channelID uint8, packet *Packet) error { return nil }

type Host struct{}

func (h *Host) Broadcast(channelID uint8, packet *Packet) error { return nil }

type EventType uint8

type Event struct {
	Type      EventType
	Peer      *Peer
	ChannelID uint8
	Data      uint32
	Packet    *Packet
}
