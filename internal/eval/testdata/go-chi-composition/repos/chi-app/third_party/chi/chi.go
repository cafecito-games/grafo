package chi

import "net/http"

type Router interface {
	http.Handler
	Use(...func(http.Handler) http.Handler)
	With(...func(http.Handler) http.Handler) Router
	Group(func(Router)) Router
	Route(string, func(Router)) Router
	Mount(string, http.Handler)
	Get(string, http.HandlerFunc)
	Post(string, http.HandlerFunc)
	MethodFunc(string, string, http.HandlerFunc)
	HandleFunc(string, http.HandlerFunc)
}

type Mux struct{}

func NewRouter() *Mux                                       { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request)   {}
func (*Mux) Use(...func(http.Handler) http.Handler)         {}
func (*Mux) With(...func(http.Handler) http.Handler) Router { return &Mux{} }
func (*Mux) Group(func(Router)) Router                      { return &Mux{} }
func (*Mux) Route(string, func(Router)) Router              { return &Mux{} }
func (*Mux) Mount(string, http.Handler)                     {}
func (*Mux) Get(string, http.HandlerFunc)                   {}
func (*Mux) Post(string, http.HandlerFunc)                  {}
func (*Mux) MethodFunc(string, string, http.HandlerFunc)    {}
func (*Mux) HandleFunc(string, http.HandlerFunc)            {}
