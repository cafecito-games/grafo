package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// TestUnknownCommandSuggestsNearMatches checks that a near miss points at the
// real spelling. A partial name is the common miss, so it must rank ahead of
// edit-distance matches, and a command that resembles nothing falls back to the
// help text alone.
func TestUnknownCommandSuggestsNearMatches(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
		absent  []string
	}{
		{
			name:    "a partial name finds the command that contains it",
			command: "cache",
			want:    []string{`unknown command "cache"`, `did you mean "embed-cache"?`},
		},
		{
			name:    "a transposition finds the command",
			command: "stauts",
			want:    []string{`did you mean "status"?`},
		},
		{
			name:    "a missing character finds the command",
			command: "neighbor",
			want:    []string{`did you mean "neighbors"?`},
		},
		{
			name:    "several near matches are all offered",
			command: "find-",
			want:    []string{`did you mean "find"`, `"find-handler"`, `"find-tests"`},
		},
		{
			name:    "an unrelated command suggests nothing",
			command: "nope",
			want:    []string{`unknown command "nope" (run 'grafo help')`},
			absent:  []string{"did you mean"},
		},
		{
			name:    "a toolchain namespace is part of the vocabulary",
			command: "godot-composition",
			want:    []string{`did you mean "godot"?`},
		},
		{
			name:    "aliases are not offered in place of canonical names",
			command: "test-coverag",
			want:    []string{`did you mean "test-coverage"?`},
			absent:  []string{"get_test_coverage", "get-test-coverage"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := New(&stdout, &stderr).Run(context.Background(), []string{test.command})
			if code != 1 {
				t.Fatalf("unknown command should exit 1, got %d", code)
			}
			for _, fragment := range test.want {
				if !strings.Contains(stderr.String(), fragment) {
					t.Fatalf("stderr %q does not contain %q", stderr.String(), fragment)
				}
			}
			for _, fragment := range test.absent {
				if strings.Contains(stderr.String(), fragment) {
					t.Fatalf("stderr %q should not contain %q", stderr.String(), fragment)
				}
			}
		})
	}
}

// TestUnknownCommandSuggestionsAreBounded keeps the error readable: a command
// that is close to everything still names only a few alternatives, in a
// deterministic order.
func TestUnknownCommandSuggestionsAreBounded(t *testing.T) {
	suggestions := suggestCommands("find")
	if len(suggestions) > maximumSuggestions {
		t.Fatalf("got %d suggestions, want at most %d: %v", len(suggestions), maximumSuggestions, suggestions)
	}
	again := suggestCommands("find")
	if !slices.Equal(suggestions, again) {
		t.Fatalf("suggestions are not deterministic: %v then %v", suggestions, again)
	}
}

// TestCommandVocabularyMatchesDispatch keeps the suggestion vocabulary and the
// dispatch table from drifting apart, since a command missing from the
// vocabulary is invisible to suggestions.
func TestCommandVocabularyMatchesDispatch(t *testing.T) {
	if !slices.IsSorted(commandVocabulary) {
		t.Fatalf("vocabulary must be sorted for deterministic suggestions: %v", commandVocabulary)
	}
	for _, entry := range commandTable {
		if len(entry.names) == 0 {
			t.Fatal("every dispatch entry needs at least one name")
		}
		if entry.handler == nil {
			t.Fatalf("command %q has no handler", entry.names[0])
		}
		if !slices.Contains(commandVocabulary, entry.names[0]) {
			t.Fatalf("canonical command %q is missing from the vocabulary", entry.names[0])
		}
		for _, name := range entry.names {
			if commandHandlers[name] == nil {
				t.Fatalf("command name %q is not dispatched", name)
			}
		}
	}
	for namespace := range toolchainNamespaces {
		if !slices.Contains(commandVocabulary, namespace) {
			t.Fatalf("toolchain namespace %q is missing from the vocabulary", namespace)
		}
		if commandHandlers[namespace] != nil {
			t.Fatalf("namespace %q must route through toolchainCommand, not the command table", namespace)
		}
	}
	for _, name := range []string{"help", "version"} {
		if !slices.Contains(commandVocabulary, name) {
			t.Fatalf("%q is accepted but missing from the vocabulary", name)
		}
	}
}
