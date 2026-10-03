package indexer_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestUnusedDeclarationElidesImporterWrites is the behaviour this change
// exists for. `middle` imports `base`, so adding a declaration to `base`
// invalidates and reparses `middle` — but `middle` never names the new
// declaration, so its evidence is unchanged and its rows must not be rewritten.
func TestUnusedDeclarationElidesImporterWrites(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)

	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	report := runEquivalenceIndex(t, ctx, project, project.IndexPath)

	// The importer is still reported as acted on, because invalidation did
	// reach it; what changed is that its rows were not rewritten.
	updated := map[string]bool{}
	for _, path := range report.Updated {
		updated[path] = true
	}
	if !updated["middle/middle.go"] {
		t.Fatalf("the importer was not reparsed; updated = %v", report.Updated)
	}
	if report.EvidenceUnchanged == 0 {
		t.Fatalf("no write was elided for an unused declaration; updated = %v, elided = %d",
			report.Updated, report.EvidenceUnchanged)
	}
	if report.EvidenceUnchanged >= len(report.Updated) {
		t.Fatalf("every write was elided, including the edited file's: elided = %d of %d",
			report.EvidenceUnchanged, len(report.Updated))
	}
}

// TestElidedWriteStillRecordsTheFile is the hazard that would turn a saving
// into a permanent cost: the file was selected because its input hash moved, so
// if the elided path skipped the file row too, the stale hash would reselect
// and reparse it on every later run.
func TestElidedWriteStillRecordsTheFile(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)
	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	elided := runEquivalenceIndex(t, ctx, project, project.IndexPath)
	if elided.EvidenceUnchanged == 0 {
		t.Fatal("the edit elided no writes, so this proves nothing about the file record")
	}

	settled := runEquivalenceIndex(t, ctx, project, project.IndexPath)
	if len(settled.Updated) != 0 {
		t.Fatalf("a refresh after an elided one reparsed %v; the elided files' records were not updated",
			settled.Updated)
	}
	if settled.EvidenceUnchanged != 0 {
		t.Fatalf("a settled refresh elided %d writes, so it was still reparsing files",
			settled.EvidenceUnchanged)
	}
}

// TestElidedFileStaysInMembership covers the other way an elided write could
// corrupt the index. Removal is decided by the indexed set minus the files this
// run accounted for, so an elided file that is not accounted for is deleted
// from the graph.
func TestElidedFileStaysInMembership(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)
	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	report := runEquivalenceIndex(t, ctx, project, project.IndexPath)

	if report.EvidenceUnchanged == 0 {
		t.Fatal("the edit elided no writes, so this proves nothing about membership")
	}
	if len(report.Removed) != 0 {
		t.Fatalf("an elided file was reported removed: %v", report.Removed)
	}
	for _, table := range []string{"nodes", "facts", "edges"} {
		if fingerprint := tableFingerprint(t, ctx, project.IndexPath, table); fingerprint == "" {
			t.Fatalf("%s is empty after an elided refresh", table)
		}
	}
}

// TestEvidenceChangingEditIsStillWritten is the negative control. An elision
// that triggered when the evidence had actually changed would leave a stale
// graph, so an importer that names the new declaration must be rewritten.
func TestEvidenceChangingEditIsStillWritten(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)

	// base gains a declaration and middle calls it, so middle's own evidence
	// gains a fact and cannot be elided.
	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
		"middle/middle.go": "package middle\n\nimport \"example.com/equivalence/base\"\n\n" +
			"func Middle() string { return base.Base() }\n\nfunc Uses() int { return base.Added() }\n",
	})
	report := runEquivalenceIndex(t, ctx, project, project.IndexPath)

	for _, path := range []string{"base/base.go", "middle/middle.go"} {
		found := false
		for _, updated := range report.Updated {
			if updated == path {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was not acted on; updated = %v", path, report.Updated)
		}
	}
	// Both files' evidence changed, so neither write may have been elided.
	if report.EvidenceUnchanged >= 2 {
		t.Fatalf("writes were elided for files whose evidence changed: elided = %d of %v",
			report.EvidenceUnchanged, report.Updated)
	}
}

// recordBlindRepository hides the file-record capability while forwarding
// everything else, which is how a storage adapter that does not offer it
// behaves. Such an adapter must index exactly as before.
type recordBlindRepository struct{ graph.IndexRepository }

func TestRepositoryWithoutTheCapabilityWritesEverything(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	index := func(blind bool) indexer.Report {
		t.Helper()
		opened, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
		}()
		var repository graph.IndexRepository = opened
		if blind {
			repository = recordBlindRepository{IndexRepository: opened}
		}
		report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	index(true)
	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	report := index(true)

	if report.EvidenceUnchanged != 0 {
		t.Fatalf("a repository without the capability elided %d writes", report.EvidenceUnchanged)
	}
	if len(report.Updated) == 0 {
		t.Fatal("a repository without the capability wrote nothing for an edit that invalidates importers")
	}
	// And it must settle: the file records are still written by the ordinary
	// replace path, so a following refresh reparses nothing.
	if settled := index(true); len(settled.Updated) != 0 {
		t.Fatalf("a refresh after an unelided one reparsed %v", settled.Updated)
	}
}

// TestElidedRefreshMatchesAColdIndexRowForRow is the equivalence that makes
// elision safe to ship: the graph after an elided incremental refresh must be
// indistinguishable from a cold index of the same tree.
func TestElidedRefreshMatchesAColdIndexRowForRow(t *testing.T) {
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)
	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	incremental := runEquivalenceIndex(t, ctx, project, project.IndexPath)
	if incremental.EvidenceUnchanged == 0 {
		t.Fatal("the edit elided no writes, so this is not testing elision")
	}

	coldPath := testtemp.Dir(t) + "/cold.sqlite"
	runEquivalenceIndex(t, ctx, project, coldPath)

	// The graph tables must agree exactly. The files table is deliberately not
	// compared: indexed_at is a timestamp, so two indexes written at different
	// moments differ there by design.
	for _, table := range []string{"nodes", "facts", "edges"} {
		incrementalRows := tableFingerprint(t, ctx, project.IndexPath, table)
		coldRows := tableFingerprint(t, ctx, coldPath, table)
		if incrementalRows != coldRows {
			missing, extra := rowDifference(incrementalRows, coldRows)
			t.Fatalf("%s differs after an elided refresh; missing %v, extra %v", table, missing, extra)
		}
	}
}
