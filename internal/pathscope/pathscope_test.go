package pathscope_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/pathscope"
)

func TestNormalizePrefixUsesRepositoryRelativeSegmentSemantics(t *testing.T) {
	tests := []struct {
		input, want string
		valid       bool
	}{
		{input: "./internal/app/", want: "internal/app", valid: true},
		{input: "internal//app", want: "internal/app", valid: true},
		{input: "", valid: false},
		{input: "../internal", valid: false},
		{input: "internal/../cmd", valid: false},
		{input: "/internal", valid: false},
		{input: `internal\app`, valid: false},
		{input: "internal/*", valid: false},
		{input: "C:/internal", valid: false},
	}
	for _, test := range tests {
		got, err := pathscope.NormalizePrefix(test.input)
		if test.valid && (err != nil || got != test.want) {
			t.Fatalf("NormalizePrefix(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
		if !test.valid && err == nil {
			t.Fatalf("NormalizePrefix(%q) unexpectedly succeeded as %q", test.input, got)
		}
	}
	if !pathscope.HasPrefix("internal/app/main.go", "internal/app") {
		t.Fatal("descendant did not match prefix")
	}
	if pathscope.HasPrefix("internal/application/main.go", "internal/app") {
		t.Fatal("non-segment prefix matched")
	}
}

func TestGlobSupportsOnlyDocumentedSlashPatterns(t *testing.T) {
	tests := []struct {
		pattern string
		matches []string
		misses  []string
	}{
		{pattern: "internal/**", matches: []string{"internal/a.go", "internal/eval/testdata/a.go"}, misses: []string{"internalized/a.go"}},
		{pattern: "cmd/*/?.go", matches: []string{"cmd/grafo/a.go"}, misses: []string{"cmd/grafo/main.go", "cmd/a/b/c.go"}},
		{pattern: "**/testdata/**", matches: []string{"internal/eval/testdata/a.go", "testdata/a.go"}, misses: []string{"internal/testdatabase/a.go"}},
	}
	for _, test := range tests {
		normalized, err := pathscope.NormalizeGlob(test.pattern)
		if err != nil {
			t.Fatalf("NormalizeGlob(%q): %v", test.pattern, err)
		}
		for _, candidate := range test.matches {
			if !pathscope.MatchGlob(normalized, candidate) {
				t.Errorf("%q did not match %q", candidate, normalized)
			}
		}
		for _, candidate := range test.misses {
			if pathscope.MatchGlob(normalized, candidate) {
				t.Errorf("%q unexpectedly matched %q", candidate, normalized)
			}
		}
	}
	for _, invalid := range []string{"", "../**", "internal/../cmd/**", "/internal/**", `internal\**`, "internal/[ab]", "internal/**foo", "C:/src/**"} {
		if got, err := pathscope.NormalizeGlob(invalid); err == nil {
			t.Errorf("NormalizeGlob(%q) unexpectedly succeeded as %q", invalid, got)
		}
	}
}
