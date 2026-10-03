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
