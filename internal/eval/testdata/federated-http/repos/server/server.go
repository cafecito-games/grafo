package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func Handler(http.ResponseWriter, *http.Request)     {}
func WrongMethod(http.ResponseWriter, *http.Request) {}
func AmbiguousA(http.ResponseWriter, *http.Request)  {}
func AmbiguousB(http.ResponseWriter, *http.Request)  {}

func Routes() {
	router := chi.NewRouter()
	router.Get("/charge/{chargeID}", Handler)
	router.Post("/charge/{chargeID}", WrongMethod)
	router.Get("/ambiguous/{leftID}", AmbiguousA)
	router.Get("/ambiguous/{rightID}", AmbiguousB)
}
