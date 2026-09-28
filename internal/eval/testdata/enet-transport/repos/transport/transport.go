package transport

import (
	enet "github.com/codecat/go-enet"
	generated "example.com/transport/generated/go/proto"
	wire "google.golang.org/protobuf/proto"
)

const gameplayChannel = 3

func send(peer enet.Peer, payload []byte) {
	_ = peer.SendBytes(payload, gameplayChannel, enet.PacketFlagReliable)
}

func SendEnvelope(peer enet.Peer, message *generated.Envelope) {
	payload, _ := wire.Marshal(message)
	send(peer, payload)
}

func ReceiveEnvelope(event enet.Event, message *generated.Envelope) {
	packet := event.GetPacket()
	payload := packet.GetData()
	_ = event.GetChannelID()
	_ = wire.Unmarshal(payload, message)
}
