package indexer_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// groupingRepository offers the grouped replacement capability on top of the
// recording repository, so one run can be compared against the same run without
// it. Every grouped file is still recorded through ReplaceFile, which is what
// makes the two durable call sequences directly comparable.
type groupingRepository struct {
	*recordingRepository
	groups []int
}

func (r *groupingRepository) ReplaceFiles(ctx context.Context, replacements []graph.FileReplacement) error {
	r.mu.Lock()
	r.groups = append(r.groups, len(replacements))
	r.mu.Unlock()
	for _, replacement := range replacements {
		if err := r.ReplaceFile(ctx, replacement.File, replacement.Parsed); err != nil {
			return err
		}
	}
	return nil
}

func (r *groupingRepository) groupSizes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.groups...)
}

func runGroupedCorpus(t *testing.T, repository graph.IndexRepository, boundary indexer.BoundaryHook) indexer.Report {
	t.Helper()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writePipelineCorpus(t, root)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.ID = "repo:grouped"
	project.Name = "corpus"
	report, err := indexer.NewService(repository, pipelineRegistry()).Run(ctx, project,
		indexer.Options{MaxFileSize: pipelineMaxFileSize, Boundary: boundary})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// TestGroupedPersistenceMatchesPerFilePersistence is the equivalence this
// optimization has to hold: grouping files into one commit may change how many
// transactions a run opens, never which evidence it stores or the order the
// report records it in.
func TestGroupedPersistenceMatchesPerFilePersistence(t *testing.T) {
	t.Parallel()
	perFile := newRecordingRepository()
	perFileReport := runGroupedCorpus(t, perFile, nil)
	grouping := &groupingRepository{recordingRepository: newRecordingRepository()}
	groupedReport := runGroupedCorpus(t, grouping, nil)

	for _, comparison := range []struct {
		name        string
		left, right any
	}{
		{name: "updated", left: perFileReport.Updated, right: groupedReport.Updated},
		{name: "skipped", left: perFileReport.Skipped, right: groupedReport.Skipped},
		{name: "removed", left: perFileReport.Removed, right: groupedReport.Removed},
		{name: "diagnostics", left: perFileReport.Diagnostics, right: groupedReport.Diagnostics},
		{name: "durable calls", left: perFile.calls(), right: grouping.calls()},
	} {
		left, err := json.MarshalIndent(comparison.left, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		right, err := json.MarshalIndent(comparison.right, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Errorf("grouped %s differ:\nper file:\n%s\ngrouped:\n%s", comparison.name, left, right)
		}
	}
	if perFileReport.Checked != groupedReport.Checked || perFileReport.Unchanged != groupedReport.Unchanged {
		t.Errorf("grouped counters differ: per file checked=%d unchanged=%d, grouped checked=%d unchanged=%d",
			perFileReport.Checked, perFileReport.Unchanged, groupedReport.Checked, groupedReport.Unchanged)
	}
}

// TestGroupedPersistenceSharesOneCommit covers the point of the capability: the
// corpus has to reach storage in fewer durable steps than it has files, and
// every persisted file has to be accounted for by exactly one of them.
func TestGroupedPersistenceSharesOneCommit(t *testing.T) {
	t.Parallel()
	grouping := &groupingRepository{recordingRepository: newRecordingRepository()}
	report := runGroupedCorpus(t, grouping, nil)
	groups := grouping.groupSizes()
	if len(groups) == 0 {
		t.Fatal("a repository offering grouped replacement was never asked to group")
	}
	if len(groups) >= len(report.Updated) {
		t.Fatalf("%d files were persisted in %d commits, want fewer commits than files",
			len(report.Updated), len(groups))
	}
	total := 0
	for _, size := range groups {
		total += size
	}
	if total != len(report.Updated) {
		t.Fatalf("grouped commits covered %d files, want the %d the report updated", total, len(report.Updated))
	}
}

// TestBoundaryObserverOptsOutOfGrouping pins the contract that makes grouping
// safe to add: a boundary observer is promised one durable boundary per
// persisted file, so a run that installs one keeps its commit per file.
func TestBoundaryObserverOptsOutOfGrouping(t *testing.T) {
	t.Parallel()
	grouping := &groupingRepository{recordingRepository: newRecordingRepository()}
	var persisted int
	report := runGroupedCorpus(t, grouping, func(boundary indexer.Boundary) error {
		if boundary.Kind == indexer.BoundaryFilePersisted {
			persisted++
		}
		return nil
	})
	if sizes := grouping.groupSizes(); len(sizes) != 0 {
		t.Fatalf("grouped %v commits while a boundary observer was installed", sizes)
	}
	if persisted != len(report.Updated) {
		t.Fatalf("observed %d file boundaries for %d updated files", persisted, len(report.Updated))
	}
}
