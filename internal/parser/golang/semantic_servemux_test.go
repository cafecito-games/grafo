package golang

import (
	"net/http"
	"testing"
)

func TestParseServeMuxPatternUsesGo122Grammar(t *testing.T) {
	tests := []struct {
		name, input, method, host, route string
		pathOnly, catchAll               bool
	}{
		{name: "path only", input: "/health", method: "ANY", route: "/health", pathOnly: true},
		{name: "method", input: "GET /api/agents", method: "GET", route: "/api/agents"},
		{name: "tabs", input: "PATCH\t\t/v1/map", method: "PATCH", route: "/v1/map"},
		{name: "host path", input: "example.test/status", method: "ANY", host: "example.test", route: "/status", pathOnly: true},
		{name: "host method wildcard", input: "DELETE example.test/items/{id}", method: "DELETE", host: "example.test", route: "/items/{id}"},
		{name: "catchall", input: "GET /assets/{path...}", method: "GET", route: "/assets/{path...}", catchAll: true},
		{name: "trailing slash catchall", input: "/assets/", method: "ANY", route: "/assets/{_...}", pathOnly: true, catchAll: true},
		{name: "end marker", input: "GET /posts/{$}", method: "GET", route: "/posts/{$}"},
		{name: "case-sensitive method", input: "get /lower", method: "get", route: "/lower"},
		{name: "literal query and fragment", input: "GET /literal?query#fragment", method: "GET", route: "/literal%3Fquery%23fragment"},
		{name: "literal trailing space", input: "GET /trail ", method: "GET", route: "/trail%20"},
		{name: "invalid escape preserved", input: "GET /bad/%zz", method: "GET", route: "/bad/%25zz"},
		{name: "escaped slash stays in segment", input: "GET /files/a%2fb", method: "GET", route: "/files/a%2Fb"},
		{name: "path-only dot segments accepted", input: "/a/../b", method: "ANY", route: "/a/%252E%252E/b", pathOnly: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pattern, err := parseServeMuxPattern(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if pattern.method != test.method || pattern.host != test.host || pattern.route != test.route ||
				pattern.pathOnly != test.pathOnly || pattern.catchAll != test.catchAll || pattern.raw != test.input {
				t.Fatalf("pattern = %#v", pattern)
			}
		})
	}
}

func TestParseServeMuxPatternMatchesStandardLibraryAcceptance(t *testing.T) {
	patterns := []string{
		"get /lower", "GET /trail ", "GET /literal?query#fragment", "GET /bad/%zz",
		"GET /files/a%2fb", "/a/../b", "CONNECT /a/../b", "GET /a/../b",
		"GET /bad/{id}/{id}", "GET /bad/{path...}/tail", "GET host/{name}",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			_, err := parseServeMuxPattern(pattern)
			if got, want := err == nil, standardServeMuxAccepts(pattern); got != want {
				t.Fatalf("parse acceptance = %v (%v), standard library = %v", got, err, want)
			}
		})
	}
}

func standardServeMuxAccepts(pattern string) (accepted bool) {
	defer func() {
		if recover() != nil {
			accepted = false
		}
	}()
	http.NewServeMux().HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return true
}

func TestParseServeMuxPatternRejectsInvalidEvidence(t *testing.T) {
	for _, input := range []string{
		"", "GET", "GET relative", "G@T /bad", "GET /a/../b", "GET {host}/path",
		"GET /bad/{", "GET /bad/x{id}", "GET /bad/{id}tail", "GET /bad/{...}",
		"GET /bad/{9id}", "GET /bad/{bad-name}", "GET /bad/{id}/{id}",
		"GET /bad/{path...}/tail", "GET /bad/{$}/tail", "GET /a/../b",
	} {
		t.Run(input, func(t *testing.T) {
			if pattern, err := parseServeMuxPattern(input); err == nil {
				t.Fatalf("parseServeMuxPattern(%q) = %#v", input, pattern)
			}
		})
	}
}
