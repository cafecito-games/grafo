package httpmodel

import (
	"fmt"
	"testing"
)

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
		{name: "ServeMux end marker", input: "/posts/{$}", canonical: "/posts/{$}"},
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
		{name: "catchall accepts trailing slash", declaration: "/assets/{path...}", request: "/assets/", want: RankCatchAll},
		{name: "ServeMux end marker requires trailing slash", declaration: "/posts/{$}", request: "/posts/", want: RankTemplate},
		{name: "ServeMux end marker rejects no slash", declaration: "/posts/{$}", request: "/posts", want: RankNone},
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

func TestDestinationContractFailsClosedOnConflictingEvidence(t *testing.T) {
	local := mustRoute(t, "/users")
	external := mustRoute(t, "https://api.example.test/users")
	tests := []struct {
		name       string
		properties map[string]string
		route      Route
	}{
		{name: "unknown is not a boolean", properties: map[string]string{PropertyAuthorityUnknown: "maybe"}, route: local},
		{name: "explicit and unknown", properties: map[string]string{PropertyAuthority: "api.example.test", PropertyAuthorityUnknown: "true"}, route: local},
		{name: "route and property authority disagree", properties: map[string]string{PropertyAuthority: "other.test"}, route: external},
		{name: "resolution lacks evidence", properties: map[string]string{PropertyDestinationResolution: string(DestinationResolved)}, route: local},
		{name: "unknown route resolution", properties: map[string]string{PropertyDestinationResolution: "guessed", PropertyDestinationEvidence: string(EvidenceRoute)}, route: local},
		{name: "unknown authority resolved by route", properties: map[string]string{PropertyAuthorityUnknown: "true", PropertyDestinationResolution: string(DestinationResolved), PropertyDestinationEvidence: string(EvidenceRoute)}, route: local},
		{name: "explicit authority marked local", properties: map[string]string{PropertyAuthority: "api.example.test", PropertyDestinationResolution: string(DestinationResolved), PropertyDestinationEvidence: string(EvidenceExactTarget)}, route: local},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseDestinationContract(test.properties, test.route); err == nil {
				t.Fatalf("corrupt contract succeeded: %#v", test.properties)
			}
		})
	}
}

func TestDestinationContractRequiresProofForLocalTargets(t *testing.T) {
	route := mustRoute(t, "/users")
	legacy, err := ParseDestinationContract(nil, route)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.ValidateTarget(false); err == nil {
		t.Fatal("legacy local target without proof was trusted")
	}
	if err := legacy.ValidateTarget(true); err != nil {
		t.Fatalf("legacy external boundary did not remain unresolved: %v", err)
	}
	proven, err := ParseDestinationContract(map[string]string{
		PropertyAuthorityUnknown:      "true",
		PropertyDestinationResolution: string(DestinationResolved),
		PropertyDestinationEvidence:   string(EvidenceExactTarget),
	}, route)
	if err != nil {
		t.Fatal(err)
	}
	if err := proven.ValidateTarget(false); err != nil {
		t.Fatalf("exact local proof was rejected: %v", err)
	}
}

func TestBestCandidateIndexesKeepsOnlyTheStrongestCompatibleRank(t *testing.T) {
	request := mustRoute(t, "/users/42")
	candidates := []EndpointCandidate{
		{Method: "POST", Route: mustRoute(t, "/users/42")},
		{Method: "GET", Route: mustRoute(t, "/users/{id}")},
		{Method: "GET", Route: mustRoute(t, "/users/42")},
		{Method: "GET", Route: mustRoute(t, "/users/{name}")},
	}
	indexes := BestCandidateIndexes("GET", request, candidates)
	if len(indexes) != 1 || indexes[0] != 2 {
		t.Fatalf("best candidate indexes = %v, want [2]", indexes)
	}
}

func TestCandidateCatalogReusesMethodIndexWithoutNarrowingRoutes(t *testing.T) {
	candidates := []EndpointCandidate{}
	for index := 0; index < 1_000; index++ {
		candidates = append(candidates, EndpointCandidate{Method: "POST", Route: mustRoute(t, fmt.Sprintf("/noise/%d", index))})
	}
	candidates = append(candidates,
		EndpointCandidate{Method: "GET", Route: mustRoute(t, "/users/{id}")},
		EndpointCandidate{Method: "GET", Route: mustRoute(t, `/codes/{id:[0-9]+}`)},
		EndpointCandidate{Method: "GET", Route: mustRoute(t, "/assets/{path...}")},
	)
	catalog := NewCandidateCatalog(candidates)
	for route, want := range map[string]int{
		"/users/42":           1_000,
		"/codes/42":           1_001,
		"/assets/css/app.css": 1_002,
	} {
		indexes := catalog.BestCandidateIndexes("GET", mustRoute(t, route))
		if len(indexes) != 1 || indexes[0] != want {
			t.Fatalf("catalog match for %s = %v, want [%d]", route, indexes, want)
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
