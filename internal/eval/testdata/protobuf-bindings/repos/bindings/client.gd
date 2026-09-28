class_name Client
extends Node

class Lookalike:
	func set_text(_value: String) -> void:
		pass
	func get_text() -> String:
		return ""
	func to_bytes() -> PackedByteArray:
		return PackedByteArray()

func make_envelope(data: PackedByteArray, typed: AcmeV1EnvelopeEnvelope) -> Variant:
	var message = AcmeV1EnvelopeEnvelope.new()
	message.set_text("hello")
	if typed.has_text():
		var text = typed.get_text()
		message.set_text(text)
	var encoded = message.to_bytes()
	var decoded = AcmeV1EnvelopeEnvelope.from_bytes(data)
	decoded.set_text("decoded")
	return decoded

func forbidden(dynamic, variant: Variant, local: Lookalike) -> void:
	dynamic.set_text("unknown")
	variant.get_text()
	local.set_text("local")
	local.get_text()
	local.to_bytes()
