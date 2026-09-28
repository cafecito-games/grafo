class_name GodotClient

const CHARGE_ROOT = "/charge/"

func call(api: AuthAPI) -> void:
	var charge_id = "{requestID}"
	var route = CHARGE_ROOT + ("%s" % charge_id) + "?view=full"
	api.request_json(HTTPClient.METHOD_GET, route)
