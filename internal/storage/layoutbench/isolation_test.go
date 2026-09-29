package layoutbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProductionBinariesExcludeLayoutbench locks the package's isolation
// contract: layoutbench is benchmark-only, so neither the production CLI nor
// the corpus benchmark binary may depend on it, not even transitively. The
// check walks the real dependency graph with go list rather than grepping
// source, so a future import added anywhere in the production subtree fails
// here no matter how deep.
func TestProductionBinariesExcludeLayoutbench(t *testing.T) {
	if testing.Short() {
		t.Skip("go list dependency walk skipped in short mode")
	}
	_, thisFile, _, callerOK := runtime.Caller(0)
	if !callerOK {
		t.Fatal("cannot locate the layoutbench source directory")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	// internal/storage/layoutbench -> internal/storage -> internal -> grafo root.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository root at %s: %v", root, err)
	}
	command := exec.CommandContext(context.Background(), "go", "list", "-deps",
		"./cmd/grafo", "./cmd/grafo-benchmark")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -deps: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("go list -deps: %v", err)
	}
	const layoutbenchPath = "github.com/cafecito-games/grafo/internal/storage/layoutbench"
	for _, line := range strings.Split(string(output), "\n") {
		dependency := strings.TrimSpace(line)
		if dependency == "" {
			continue
		}
		if dependency == layoutbenchPath || strings.HasPrefix(dependency, layoutbenchPath+"/") {
			t.Fatalf("production binary dependency %s imports the benchmark-only layoutbench package",
				dependency)
		}
	}
}
