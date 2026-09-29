package detailprofile_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/detailprofile"
)

func TestBenchmarkProducesComparableRawProfileEvidence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app.py"), ""+
		"def handle(value):\n"+
		"    local = value\n"+
		"    return local\n")
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.name", "Detail Profile Test")
	runGit(t, root, "config", "user.email", "detail@example.invalid")
	runGit(t, root, "add", "app.py")
	runGit(t, root, "commit", "-m", "fixture")
	output := t.TempDir()

	full, err := detailprofile.RunBenchmark(context.Background(), detailprofile.BenchmarkOptions{
		Repository: root, Output: output, Profile: detailprofile.ProfileFull, Samples: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if full.Summary.Samples != 1 || full.Summary.MedianCompactedBytes <= 0 || full.Samples[0].Cold.Report.Counts.Files != 1 {
		t.Fatalf("full report = %#v", full)
	}
	if full.Samples[0].Database != "" || full.Samples[0].Projection.InputNodes != full.Samples[0].Projection.OutputNodes {
		t.Fatalf("full projection/artifact = %#v", full.Samples[0])
	}

	structural, err := detailprofile.RunBenchmark(context.Background(), detailprofile.BenchmarkOptions{
		Repository: root, Output: output, Profile: detailprofile.ProfileStructural, Samples: 1, Baseline: full.Artifacts.Report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if structural.Comparison == nil || !structural.Comparison.ClaimedEquivalent {
		t.Fatalf("structural comparison = %#v", structural.Comparison)
	}
	if structural.Samples[0].Projection.OutputNodes >= structural.Samples[0].Projection.InputNodes {
		t.Fatalf("structural projection did not reduce local evidence: %#v", structural.Samples[0].Projection)
	}
}

func TestProductionCompositionDoesNotImportDetailProfile(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "./cmd/grafo", "./internal/cli", "./internal/mcpserver", "./internal/service")
	command.Dir = repositoryRoot(t)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "github.com/cafecito-games/grafo/internal/detailprofile") {
		t.Fatal("production composition imports benchmark-only detailprofile package")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(directory, "..", ".."))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
