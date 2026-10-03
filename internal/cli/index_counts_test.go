package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestIndexCollectsFullGraphCountsOnlyWhenRequested pins the reporting contract
// for 'grafo index': the full-graph count queries scale with total graph size,
// so they run only when the invocation asked for them, and a suppressed summary
// is reported as uncollected rather than as zero totals.
func TestIndexCollectsFullGraphCountsOnlyWhenRequested(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module sample\n\ngo 1.26\n")
	write("charge.go", "package sample\n\nfunc Charge() error { return nil }\n")

	code, stdout, stderr := execute(t, "index", root, "--json")
	if code != 0 {
		t.Fatalf("index exited with %d: %s", code, stderr)
	}
	var suppressed map[string]any
	if err := json.Unmarshal([]byte(stdout), &suppressed); err != nil {
		t.Fatalf("decode index report: %v (%s)", err, stdout)
	}
	if collected, _ := suppressed["counts_collected"].(bool); collected {
		t.Fatalf("index collected counts without --counts: %s", stdout)
	}
	if _, exists := suppressed["counts"]; exists {
		t.Fatalf("suppressed counts were reported as real totals: %s", stdout)
	}

	code, stdout, stderr = execute(t, "index", root, "--counts", "--json")
	if code != 0 {
		t.Fatalf("index --counts exited with %d: %s", code, stderr)
	}
	var requested struct {
		CountsCollected bool `json:"counts_collected"`
		Counts          *struct {
			Files int `json:"files"`
			Nodes int `json:"nodes"`
		} `json:"counts"`
	}
	if err := json.Unmarshal([]byte(stdout), &requested); err != nil {
		t.Fatalf("decode index --counts report: %v (%s)", err, stdout)
	}
	if !requested.CountsCollected || requested.Counts == nil {
		t.Fatalf("index --counts omitted the counts summary: %s", stdout)
	}
	if requested.Counts.Files == 0 || requested.Counts.Nodes == 0 {
		t.Fatalf("index --counts reported empty totals: %s", stdout)
	}
}

// TestIndexHumanReportDistinguishesSuppressedCounts keeps the text report from
// printing fabricated zero totals when counts were not collected.
func TestIndexHumanReportDistinguishesSuppressedCounts(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module sample\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "charge.go"),
		[]byte("package sample\n\nfunc Charge() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := execute(t, "index", root)
	if code != 0 {
		t.Fatalf("index exited with %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "0 nodes") || strings.Contains(stdout, "0 files ·") {
		t.Fatalf("suppressed counts printed as zero totals: %s", stdout)
	}
	if !strings.Contains(stdout, "--counts") {
		t.Fatalf("report did not say how to request counts: %s", stdout)
	}
	code, stdout, stderr = execute(t, "index", root, "--counts")
	if code != 0 {
		t.Fatalf("index --counts exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, " nodes · ") {
		t.Fatalf("index --counts omitted the counts line: %s", stdout)
	}
}
