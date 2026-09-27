class_name Player extends Node

signal finished(value: int)

func load_scene() -> String:
	finished.emit(1)
	finished.connect(on_finished)
	if finished.is_connected(on_finished):
		finished.disconnect(on_finished)
	if Input.is_action_just_pressed("jump"):
		add_to_group("enemies")
	get_tree().call_group("enemies", "done")
	get_tree().get_nodes_in_group("undeclared_group")
	done()
	Game.start()
	Disabled.start()
	return ProjectSettings.get_setting("application/run/main_scene")

func done() -> void:
	pass

func on_finished(_value: int) -> void:
	pass
