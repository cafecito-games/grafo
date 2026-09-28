package enet

type PacketFlags uint32

const PacketFlagReliable PacketFlags = 1

type Packet interface { GetData() []byte }
type Event interface {
	GetPacket() Packet
	GetChannelID() uint8
}
type Peer interface {
	SendBytes(payload []byte, channel uint8, flags PacketFlags) error
}
