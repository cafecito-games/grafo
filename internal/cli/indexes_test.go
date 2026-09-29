package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	branchindexes "github.com/cafecito-games/grafo/internal/indexes"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestIndexesOptionsAndUsageFailBeforeRepositoryAccess(t *testing.T) {
	args, err := parseArguments([]string{"indexes", "prune", "/tmp/repo", "--older-than", "24h", "--keep=3", "--dry-run", "--yes", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if args.values["older-than"] != "24h" || args.values["keep"] != "3" || !args.flags["dry-run"] || !args.flags["yes"] {
		t.Fatalf("parsed arguments = %#v", args)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	for _, test := range []struct {
		arguments []string
		message   string
	}{
		{[]string{"indexes", "prune", missing}, "at least one"},
		{[]string{"indexes", "prune", missing, "--older-than", "-1s", "--yes"}, "must not be negative"},
		{[]string{"indexes", "prune", missing, "--keep", "-1", "--yes"}, "must not be negative"},
		{[]string{"indexes", "prune", missing, "--keep", "0"}, "requires --yes"},
		{[]string{"indexes", "list", missing, "--keep", "1"}, "not supported"},
		{[]string{"indexes", "compact", missing}, "requires --yes"},
		{[]string{"indexes", "compact", missing, "--older-than", "1h", "--dry-run"}, "not supported"},
		{[]string{"indexes", "unknown", missing}, "usage: grafo indexes"},
	} {
		_, stderr, code := output(t, test.arguments...)
		if code == 0 || !strings.Contains(stderr, test.message) || strings.Contains(stderr, "resolve project root") {
			t.Fatalf("grafo %v: code=%d stderr=%q", test.arguments, code, stderr)
		}
	}
}

func TestIndexesListAndPruneTextJSONParity(t *testing.T) {
	root := t.TempDir()
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
	repository, err := sqlite.Open(context.Background(), oldPath)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"root": project.Root, "repository_id": project.ID, "branch": "old", "commit": "old-commit",
		"indexed_at":             time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano),
		"semantic_index_version": indexer.SemanticIndexVersion,
	} {
		if err := repository.SetMeta(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	jsonOutput, stderr, code := output(t, "indexes", "list", root, "--json")
	if code != 0 {
		t.Fatalf("JSON list: code=%d stderr=%q", code, stderr)
	}
	var inventory branchindexes.Inventory
	if err := json.Unmarshal([]byte(jsonOutput), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Indexes) != 2 || !inventory.Indexes[0].Current || inventory.Indexes[1].Filename != "old.sqlite" {
		t.Fatalf("JSON inventory = %#v", inventory)
	}
	if inventory.Indexes[0].Metrics == nil || inventory.Indexes[0].Metrics.PageSize <= 0 {
		t.Fatalf("JSON inventory metrics = %#v", inventory.Indexes[0].Metrics)
	}
	textOutput, stderr, code := output(t, "indexes", "list", root)
	if code != 0 {
		t.Fatalf("text list: code=%d stderr=%q", code, stderr)
	}
	currentOffset := strings.Index(textOutput, inventory.Indexes[0].Filename)
	oldOffset := strings.Index(textOutput, inventory.Indexes[1].Filename)
	if currentOffset < 0 || oldOffset <= currentOffset || !strings.Contains(textOutput, "totals") || !strings.Contains(textOutput, "database=") {
		t.Fatalf("text inventory did not preserve JSON order/totals:\n%s", textOutput)
	}
	for _, required := range []string{oldPath, project.Root, project.ID, "old-commit"} {
		if !strings.Contains(textOutput, required) {
			t.Fatalf("text inventory omitted required metadata %q:\n%s", required, textOutput)
		}
	}
	for _, required := range []string{"PAGE_SIZE", "RECLAIMABLE", "COMPACT_RECOMMENDED"} {
		if !strings.Contains(textOutput, required) {
			t.Fatalf("text inventory omitted storage column %q:\n%s", required, textOutput)
		}
	}

	beforeDryRun, err := os.ReadFile(project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	compactJSON, stderr, code := output(t, "indexes", "compact", root, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("compact dry run: code=%d stderr=%q", code, stderr)
	}
	var compact branchindexes.CompactReport
	if err := json.Unmarshal([]byte(compactJSON), &compact); err != nil {
		t.Fatal(err)
	}
	if !compact.DryRun || compact.After != nil || compact.Before.Metrics.PageCount <= 0 || compact.ExpectedUpperBoundBytes != compact.Before.Metrics.LiveAllocatedBytes {
		t.Fatalf("compact dry report = %#v", compact)
	}
	afterDryRun, err := os.ReadFile(project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeDryRun) != string(afterDryRun) {
		t.Fatal("CLI compact dry run changed primary database")
	}
	compactText, stderr, code := output(t, "indexes", "compact", root, "--yes")
	if code != 0 || !strings.Contains(compactText, "before database=") || !strings.Contains(compactText, "after database=") || !strings.Contains(compactText, "reclaimed database=") {
		t.Fatalf("compact real: code=%d stdout=%q stderr=%q", code, compactText, stderr)
	}

	dryJSON, stderr, code := output(t, "indexes", "prune", root, "--keep", "1", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dry prune: code=%d stderr=%q", code, stderr)
	}
	var dry branchindexes.PruneReport
	if err := json.Unmarshal([]byte(dryJSON), &dry); err != nil {
		t.Fatal(err)
	}
	if selectedResult(dry, "old.sqlite") == nil || !selectedResult(dry, "old.sqlite").Selected {
		t.Fatalf("dry report = %#v", dry.Results)
	}
	if selectedResult(dry, "old.sqlite").Status != branchindexes.StatusWouldDelete {
		t.Fatalf("dry-run selected status = %q, want %q", selectedResult(dry, "old.sqlite").Status, branchindexes.StatusWouldDelete)
	}
	dryText, stderr, code := output(t, "indexes", "prune", root, "--keep", "1", "--dry-run")
	if code != 0 || !strings.Contains(dryText, string(branchindexes.StatusWouldDelete)) || strings.Contains(dryText, "protected\told\told.sqlite") {
		t.Fatalf("dry text: code=%d stdout=%q stderr=%q", code, dryText, stderr)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("dry run removed old index: %v", err)
	}
	realText, stderr, code := output(t, "indexes", "prune", root, "--keep", "1", "--yes")
	if code != 0 || !strings.Contains(realText, "deleted") {
		t.Fatalf("real prune: code=%d stdout=%q stderr=%q", code, realText, stderr)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old index survived CLI prune: %v", err)
	}
}

func selectedResult(report branchindexes.PruneReport, filename string) *branchindexes.Result {
	for index := range report.Results {
		if report.Results[index].Index.Filename == filename {
			return &report.Results[index]
		}
	}
	return nil
}
