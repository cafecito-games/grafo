class_name Player extends Node

signal finished(value: int)

func load_scene() -> String:
	finished.emit(1)
	finished.connect(on_finished)
	done()
	Game.start()
	Disabled.start()
	return ProjectSettings.get_setting("application/run/main_scene")

func done() -> void:
	pass

func on_finished(_value: int) -> void:
	pass
