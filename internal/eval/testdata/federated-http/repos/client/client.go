package client

import "net/http"

func Call() {
	http.Get("/charge/{requestID}/?view=full")
}
