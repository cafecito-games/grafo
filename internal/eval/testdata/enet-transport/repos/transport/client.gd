class_name TransportClient

const GAMEPLAY_CHANNEL = 3

func send(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	peer.send(GAMEPLAY_CHANNEL, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func receive(peer: ENetPacketPeer) -> AcmeV1EnvelopeEnvelope:
	var packet = peer.get_packet()
	return AcmeV1EnvelopeEnvelope.from_bytes(packet)
