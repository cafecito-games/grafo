class_name TransportClient

const GAMEPLAY_CHANNEL = 3

func send(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	message.set_text("hello")
	message.set_image(PackedByteArray())
	peer.send(GAMEPLAY_CHANNEL, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func ambiguous_send(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, alternate: bool) -> void:
	var channel = 4
	if alternate:
		channel = 5
	peer.send(channel, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func receive(peer: ENetPacketPeer) -> AcmeV1EnvelopeEnvelope:
	var packet = peer.get_packet()
	var message = AcmeV1EnvelopeEnvelope.from_bytes(packet)
	print(message.get_text())
	print(message.get_receipt())
	return message
