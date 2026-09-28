package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkWarmStatus is deliberately compatible with the issue's base
// revision. Copy this unchanged file into either checkout and use one timed
// invocation per process sample (-benchtime=1x -count=30).
func BenchmarkWarmStatus(b *testing.B) {
	root := b.TempDir()
	for name, content := range map[string]string{
		"go.mod":    "module benchmark.example/status\n\ngo 1.26\n",
		"status.go": "package status\nfunc Ready() bool { return true }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	var setupOut, setupErr bytes.Buffer
	if code := New(&setupOut, &setupErr).Run(context.Background(), []string{"index", root}); code != 0 {
		b.Fatalf("index exited %d: %s", code, setupErr.String())
	}
	b.ResetTimer()
	for range b.N {
		var stdout, stderr bytes.Buffer
		if code := New(&stdout, &stderr).Run(context.Background(), []string{"status", root}); code != 0 {
			b.Fatalf("status exited %d: %s", code, stderr.String())
		}
		if stderr.Len() != 0 {
			b.Fatalf("default auto progress wrote to buffered stderr: %q", stderr.String())
		}
		if !strings.Contains(stdout.String(), " files · ") || !strings.Contains(stdout.String(), "indexed ") {
			b.Fatalf("status payload is incomplete: %q", stdout.String())
		}
	}
}
