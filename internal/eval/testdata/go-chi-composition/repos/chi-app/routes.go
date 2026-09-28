package chiapp

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

const api = "/v1"

func requestID(next http.Handler) http.Handler    { return next }
func authenticate(next http.Handler) http.Handler { return next }
func audit(next http.Handler) http.Handler        { return next }
func login(http.ResponseWriter, *http.Request)    {}
func profile(http.ResponseWriter, *http.Request)  {}

func authRoutes(router chi.Router) {
	router.Use(authenticate)
	router.With(audit).Post("/login", login)
	router.Get("/profile", profile)
}

func childRoutes() chi.Router {
	router := chi.NewRouter()
	router.Get("/profile", profile)
	return router
}

func Routes(dynamic string) chi.Router {
	router := chi.NewRouter()
	router.Use(requestID)
	router.Route(api+"/auth", authRoutes)
	router.Route("/staff", authRoutes)
	router.Group(func(group chi.Router) {
		group.Mount("/nested", childRoutes())
	})
	router.Mount("/child", childRoutes())
	router.MethodFunc(http.MethodPatch, "/profile", profile)
	router.HandleFunc("/fallback", profile)
	router.Route(dynamic, authRoutes)
	cycleA(router)
	return router
}

func cycleA(router chi.Router) { cycleB(router) }
func cycleB(router chi.Router) { cycleA(router) }

type unrelated struct{}

func (unrelated) Get(string, http.HandlerFunc) {}
func NotARouter()                              { unrelated{}.Get("/invented", profile) }
