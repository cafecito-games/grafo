package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestToolchainCommandRouting checks that the parent command routes to its
// subcommand, reports the full command name in that subcommand's own usage, and
// refuses a missing or unknown subcommand without touching an index.
func TestToolchainCommandRouting(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      string
	}{
		{
			name:      "missing subcommand lists the subcommands",
			arguments: []string{"godot"},
			want:      "usage: grafo godot <composition|interactions>",
		},
		{
			name:      "unknown subcommand names itself",
			arguments: []string{"godot", "nope", "scenes/main"},
			want:      `unknown godot subcommand "nope"`,
		},
		{
			name:      "subcommand usage carries the parent command",
			arguments: []string{"godot", "composition"},
			want:      "usage: grafo godot composition <scene-resource-script-or-autoload>",
		},
		{
			name:      "interactions usage carries the parent command",
			arguments: []string{"godot", "interactions"},
			want:      "usage: grafo godot interactions <scene-node-script-action-group-or-signal>",
		},
		{
			name:      "the old flat spelling is gone",
			arguments: []string{"godot-composition", "scenes/main"},
			want:      `unknown command "godot-composition"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := New(&stdout, &stderr).Run(context.Background(), test.arguments); code == 0 {
				t.Fatalf("expected a failure, got stdout %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), test.want)
			}
		})
	}
}

// TestToolchainNamespacesAreConsistent keeps the usage order and the handler
// table from drifting apart, since usage is rendered from the order alone.
func TestToolchainNamespacesAreConsistent(t *testing.T) {
	for command, namespace := range toolchainNamespaces {
		if namespace.name != command {
			t.Errorf("namespace %q is registered under %q", namespace.name, command)
		}
		if len(namespace.order) != len(namespace.subcommands) {
			t.Errorf("namespace %q orders %d of %d subcommands", command, len(namespace.order), len(namespace.subcommands))
		}
		for _, subcommand := range namespace.order {
			if namespace.subcommands[subcommand] == nil {
				t.Errorf("namespace %q orders unknown subcommand %q", command, subcommand)
			}
		}
	}
}
