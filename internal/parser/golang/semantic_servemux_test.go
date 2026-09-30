package golang

import "testing"

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

func TestParseServeMuxPatternRejectsInvalidEvidence(t *testing.T) {
	for _, input := range []string{
		"", "GET", "GET relative", "G@T /bad", "GET /a/../b", "GET {host}/path",
		"GET /bad/{", "GET /bad/x{id}", "GET /bad/{id}tail", "GET /bad/{...}",
		"GET /bad/{9id}", "GET /bad/{bad-name}", "GET /bad/{id}/{id}",
		"GET /bad/{path...}/tail", "GET /bad/{$}/tail", "GET /bad/%zz",
		"GET /bad?query=true", "GET /bad#fragment",
	} {
		t.Run(input, func(t *testing.T) {
			if pattern, err := parseServeMuxPattern(input); err == nil {
				t.Fatalf("parseServeMuxPattern(%q) = %#v", input, pattern)
			}
		})
	}
}
