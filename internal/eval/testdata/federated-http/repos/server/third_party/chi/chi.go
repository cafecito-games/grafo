package chi

import "net/http"

type Router interface {
	http.Handler
	Get(string, http.HandlerFunc)
	Post(string, http.HandlerFunc)
}

type Mux struct{}

func NewRouter() *Mux                                     { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Mux) Get(string, http.HandlerFunc)                 {}
func (*Mux) Post(string, http.HandlerFunc)                {}
