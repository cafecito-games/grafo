package storagecompare

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRunProducesRawSamplesMediansAndCorrectnessComparison(t *testing.T) {
	repository := fixtureRepository(t)
	output := filepath.Join(t.TempDir(), "comparison")
	report, err := Run(context.Background(), Options{Repository: repository, Output: output, Samples: 1, GeneratedRows: 20})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusPassed || len(report.Engines) != 3 {
		t.Fatalf("report = %#v", report)
	}
	for _, engine := range report.Engines {
		if !engine.Correctness.Valid || engine.Generated.ColdTotalNS.Median <= 0 || len(engine.Generated.ColdTotalNS.Raw) != 1 {
			t.Fatalf("engine result = %#v", engine)
		}
		cold, ok := engine.Corpus.Scenarios["cold"]
		if !ok || cold.TotalNS.Median <= 0 || len(cold.TotalNS.Raw) != 1 {
			t.Fatalf("cold corpus result = %#v", cold)
		}
	}
	if _, err := os.Stat(filepath.Join(output, ReportFileName)); err != nil {
		t.Fatal(err)
	}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	files := map[string]string{
		"go.mod":        "module example.com/storagefixture\n\ngo 1.26\n",
		"main.go":       "package fixture\nfunc Value() int { return 1 }\n",
		"worker.py":     "def run():\n    return True\n",
		"schema.sql":    "CREATE TABLE events (id bigint PRIMARY KEY);\n",
		"settings.yaml": "service:\n  enabled: true\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")
	return root
}

func runGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
