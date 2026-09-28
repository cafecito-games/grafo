package transport

import (
	generated "example.com/transport/generated/go/proto"
	enet "github.com/codecat/go-enet"
	wire "google.golang.org/protobuf/proto"
)

const gameplayChannel = 3

type localPeer struct{}
type localMessage struct{ Text string }

func (*localPeer) SendBytes([]byte, int, int) error { return nil }
func sameNameOnly(peer *localPeer, message *localMessage) {
	message.Text = "not protobuf"
	_ = peer.SendBytes([]byte(message.Text), gameplayChannel, 1)
}

func send(peer enet.Peer, payload []byte) {
	_ = peer.SendBytes(payload, gameplayChannel, enet.PacketFlagReliable)
}

func SendEnvelope(peer enet.Peer, message *generated.Envelope) {
	payload, _ := wire.Marshal(message)
	send(peer, payload)
}

func EncodeOnly(message *generated.EncodedOnly) {
	_, _ = wire.Marshal(message)
}

func ReceiveEnvelope(event enet.Event, message *generated.Envelope) {
	packet := event.GetPacket()
	payload := packet.GetData()
	_ = event.GetChannelID()
	_ = wire.Unmarshal(payload, message)
}
