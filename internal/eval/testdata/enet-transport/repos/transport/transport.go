package transport

import (
	generated "example.com/transport/generated/go/proto"
	goenet "github.com/cafecito-games/goenet/pkg"
	wire "google.golang.org/protobuf/proto"
)

const gameplayChannel = 3

type localPeer struct{}
type localMessage struct{ Text string }

func (*localPeer) Send(uint8, *goenet.Packet) error { return nil }
func sameNameOnly(peer *localPeer, message *localMessage) {
	message.Text = "not protobuf"
	_ = peer.Send(gameplayChannel, &goenet.Packet{Data: []byte(message.Text), Flags: goenet.PacketFlagReliable})
}

func send(peer *goenet.Peer, payload []byte) {
	_ = peer.Send(gameplayChannel, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}

// GamePeer narrows peer behaviour the way the uzir server does, embedding
// goenet.PeerSender so the call keeps the upstream method identity.
type GamePeer interface {
	Close() error
	goenet.PeerSender
}

func sendViaGamePeer(peer GamePeer, payload []byte) {
	_ = peer.Send(gameplayChannel, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}

func SendEnvelopeViaGamePeer(peer GamePeer, message *generated.Envelope) {
	payload, _ := wire.Marshal(message)
	sendViaGamePeer(peer, payload)
}

func SendEnvelope(peer *goenet.Peer, message *generated.Envelope) {
	payload, _ := wire.Marshal(message)
	send(peer, payload)
}

func EncodeOnly(message *generated.EncodedOnly) {
	_, _ = wire.Marshal(message)
}

func ReceiveEnvelope(event goenet.Event, message *generated.Envelope) {
	payload := event.Packet.Data
	_ = event.ChannelID
	_ = wire.Unmarshal(payload, message)
}

func DispatchEnvelope(event goenet.Event, message *generated.Envelope) {
	ReceiveEnvelope(event, message)
}
