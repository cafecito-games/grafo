class_name PlayerSpec
extends GutTest

func test_damage() -> void:
	assert_damage()

func test_direct() -> void:
	damage()

func assert_damage() -> void:
	damage()

func test_through_inferred_receiver() -> void:
	var player := _make_player()
	player.damage()

func _make_player() -> Player:
	return Player.new()

func test_signal_lifecycle_on_an_inferred_receiver() -> void:
	var player := _make_player()
	player.finished.connect(on_finished)
	player.finished.disconnect(on_finished)
	if player.finished.is_connected(on_finished):
		pass

func on_finished(_value: int) -> void:
	pass
