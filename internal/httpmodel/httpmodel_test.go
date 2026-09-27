package httpmodel

import "testing"

func TestParseRouteCanonicalizesIdentityAndPreservesEvidence(t *testing.T) {
	tests := []struct {
		name, input, canonical, query, fragment, scheme, authority string
	}{
		{name: "root", input: "/", canonical: "/"},
		{name: "trailing slash", input: "/users/", canonical: "/users"},
		{name: "query and fragment", input: "/users/?active=true#top", canonical: "/users", query: "active=true", fragment: "top"},
		{name: "absolute URL", input: "https://api.example.test/users/42?q=go#result", canonical: "/users/42", query: "q=go", fragment: "result", scheme: "https", authority: "api.example.test"},
		{name: "parameter name", input: "/users/{characterID}", canonical: "/users/{_}"},
		{name: "regex constraint", input: `/users/{characterID:[0-9]+}`, canonical: `/users/{_:[0-9]+}`},
		{name: "regex query metacharacter", input: `/users/{characterID:[0-9]?}`, canonical: `/users/{_:[0-9]?}`},
		{name: "regex repetition", input: `/users/{characterID:[0-9]{2}}`, canonical: `/users/{_:[0-9]{2}}`},
		{name: "escaped regex", input: `/users/{characterID:\d+}`, canonical: `/users/{_:\d+}`},
		{name: "catchall", input: "/assets/{path...}", canonical: "/assets/{_...}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := ParseRoute(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if route.Canonical != test.canonical || route.Query != test.query || route.Fragment != test.fragment || route.Scheme != test.scheme || route.Authority != test.authority {
				t.Fatalf("ParseRoute(%q) = %#v", test.input, route)
			}
		})
	}
}

func TestParseRouteRejectsEvidenceThatCannotBeMatchedSafely(t *testing.T) {
	for _, input := range []string{"", "users", "/bad/%zz", "/bad?value=%zz", "/users\n", "/users/{id", "/users/{id:[}", "/assets/{path...}/tail", "https://"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseRoute(input); err == nil {
				t.Fatalf("ParseRoute(%q) succeeded", input)
			}
		})
	}
}

func TestJoinCanonicalizesPrefixAndLeafWithoutCleaningSegments(t *testing.T) {
	route, err := Join("/api/", "/users/{id}/")
	if err != nil {
		t.Fatal(err)
	}
	if route.Canonical != "/api/users/{_}" {
		t.Fatalf("joined route = %#v", route)
	}
	if _, err := Join("/api/../private", "/users"); err == nil {
		t.Fatal("unsafe dot segment was accepted")
	}
}

func TestCompatibilityRanksOnlyProvenMatches(t *testing.T) {
	tests := []struct {
		name, declaration, request string
		want                       MatchRank
	}{
		{name: "exact literal", declaration: "/users/42", request: "/users/42", want: RankExact},
		{name: "different parameter names", declaration: "/users/{id}", request: "/users/{characterID}", want: RankTemplate},
		{name: "literal satisfies parameter", declaration: "/users/{id}", request: "/users/42", want: RankTemplate},
		{name: "regex accepts literal", declaration: `/users/{id:[0-9]+}`, request: "/users/42", want: RankTemplate},
		{name: "regex rejects literal", declaration: `/users/{id:[0-9]+}`, request: "/users/nope", want: RankNone},
		{name: "unknown does not satisfy regex", declaration: `/users/{id:[0-9]+}`, request: "/users/{value}", want: RankNone},
		{name: "same regex is compatible", declaration: `/users/{id:[0-9]+}`, request: `/users/{value:[0-9]+}`, want: RankTemplate},
		{name: "catchall is lowest confidence", declaration: "/assets/{path...}", request: "/assets/css/app.css", want: RankCatchAll},
		{name: "dynamic catchall needs catchall declaration", declaration: "/assets/{file}", request: "/assets/{path...}", want: RankNone},
		{name: "segment count differs", declaration: "/users/{id}", request: "/users/42/profile", want: RankNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			declaration := mustRoute(t, test.declaration)
			request := mustRoute(t, test.request)
			if got := Compatibility(declaration, request); got != test.want {
				t.Fatalf("Compatibility(%q, %q) = %v, want %v", test.declaration, test.request, got, test.want)
			}
		})
	}
}

func TestNormalizeMethodRejectsMalformedTokens(t *testing.T) {
	if method, err := NormalizeMethod(" get "); err != nil || method != "GET" {
		t.Fatalf("NormalizeMethod = %q, %v", method, err)
	}
	for _, input := range []string{"", "G ET", "GET\n", "G@T"} {
		if _, err := NormalizeMethod(input); err == nil {
			t.Fatalf("NormalizeMethod(%q) succeeded", input)
		}
	}
}

func mustRoute(t *testing.T, value string) Route {
	t.Helper()
	route, err := ParseRoute(value)
	if err != nil {
		t.Fatal(err)
	}
	return route
}
