package projectconfig_test

import (
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/projectconfig"
)

func TestParseIndexScopeNormalizesMatchesAndDigestsDeterministically(t *testing.T) {
	first, err := projectconfig.Parse([]byte("index:\n  include: [./cmd/**, internal/**]\n  exclude: [internal/eval/testdata/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := projectconfig.Parse([]byte("index:\n  exclude: [internal/eval/testdata/**]\n  include: [internal/**, cmd/**, cmd/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Index.SemanticKey() != second.Index.SemanticKey() {
		t.Fatalf("equivalent scopes differ: %q != %q", first.Index.SemanticKey(), second.Index.SemanticKey())
	}
	for path, want := range map[string]bool{
		"cmd/grafo/main.go":             true,
		"internal/query/catalog.go":     true,
		"internal/eval/testdata/app.go": false,
		"README.md":                     false,
		"grafo.yaml":                    true,
	} {
		if got := first.Index.Allows(path); got != want {
			t.Errorf("Allows(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseIndexScopeRejectsUnsafeContracts(t *testing.T) {
	tests := []struct{ source, want string }{
		{source: "index: []\n", want: "index must be a mapping"},
		{source: "index:\n  paths: []\n", want: `unknown index setting "paths"`},
		{source: "index:\n  include: cmd/**\n", want: "include must be a sequence"},
		{source: "index:\n  include: [42]\n", want: "include pattern must be a string"},
		{source: "index:\n  include: ['']\n", want: "invalid include pattern"},
		{source: "index:\n  exclude: [../secret/**]\n", want: "invalid exclude pattern"},
		{source: "index:\n  include: ['internal\\**']\n", want: "slash separators"},
		{source: "index:\n  include: [cmd/**]\n  include: [internal/**]\n", want: `duplicate index setting "include"`},
		{source: "index: {}\nindex: {}\n", want: `duplicate top-level section "index"`},
	}
	for _, test := range tests {
		_, err := projectconfig.Parse([]byte(test.source))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Parse(%q) error = %v, want %q", test.source, err, test.want)
		}
	}
}

func TestParseIndexSeedIsOptionalAndStrict(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   *bool
	}{
		{name: "unset", source: "index:\n  include: [cmd/**]\n", want: nil},
		{name: "disabled", source: "index:\n  seed: false\n", want: boolPointer(false)},
		{name: "enabled", source: "index:\n  seed: true\n", want: boolPointer(true)},
		{name: "empty", source: "index:\n  seed:\n", want: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := projectconfig.Parse([]byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.want == nil && config.IndexSeed != nil:
				t.Fatalf("seed = %t, want unset", *config.IndexSeed)
			case test.want != nil && config.IndexSeed == nil:
				t.Fatalf("seed unset, want %t", *test.want)
			case test.want != nil && *config.IndexSeed != *test.want:
				t.Fatalf("seed = %t, want %t", *config.IndexSeed, *test.want)
			}
		})
	}
	// A non-boolean seed is rejected rather than coerced, so a typo cannot
	// silently disable adoption.
	if _, err := projectconfig.Parse([]byte("index:\n  seed: yes-please\n")); err == nil {
		t.Fatal("accepted a non-boolean index seed setting")
	}
}

func boolPointer(value bool) *bool { return &value }
