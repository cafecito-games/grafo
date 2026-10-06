package indexer_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type indexRepositorySpy struct {
	graph.IndexRepository
	replaceOwnerCalls int
	removeFilesCalls  int
	reconcileCalls    int
	countsCalls       int
	setMetaKeys       []string
}

func (s *indexRepositorySpy) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	s.replaceOwnerCalls++
	return s.IndexRepository.ReplaceOwner(ctx, owner, parsed)
}

func (s *indexRepositorySpy) RemoveFiles(ctx context.Context, paths []string) error {
	s.removeFilesCalls++
	return s.IndexRepository.RemoveFiles(ctx, paths)
}

func (s *indexRepositorySpy) Reconcile(ctx context.Context) error {
	s.reconcileCalls++
	return s.IndexRepository.Reconcile(ctx)
}

func (s *indexRepositorySpy) Counts(ctx context.Context) (graph.Counts, error) {
	s.countsCalls++
	return s.IndexRepository.Counts(ctx)
}

func (s *indexRepositorySpy) SetMeta(ctx context.Context, key, value string) error {
	s.setMetaKeys = append(s.setMetaKeys, key)
	return s.IndexRepository.SetMeta(ctx, key, value)
}

func (s *indexRepositorySpy) ReconciliationPending(ctx context.Context) (bool, error) {
	return s.IndexRepository.(graph.ReconciliationStatusRepository).ReconciliationPending(ctx)
}

func TestServiceProvenUnchangedRefreshSkipsGraphWritesAndCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "README.md"), "# Sample\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	first, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.CountsCollected || first.Counts.Files != 1 {
		t.Fatalf("default report omitted counts: %#v", first)
	}

	project, err = indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	spy := &indexRepositorySpy{IndexRepository: repository}
	report, err := indexer.NewService(spy, registry).Run(ctx, project, indexer.Options{ReportDetail: indexer.ReportWithoutCounts})
	if err != nil {
		t.Fatal(err)
	}
	if spy.replaceOwnerCalls != 0 || spy.removeFilesCalls != 0 || spy.reconcileCalls != 0 || spy.countsCalls != 0 {
		t.Fatalf("unchanged refresh calls owner=%d remove=%d reconcile=%d counts=%d",
			spy.replaceOwnerCalls, spy.removeFilesCalls, spy.reconcileCalls, spy.countsCalls)
	}
	if len(spy.setMetaKeys) != 1 || spy.setMetaKeys[0] != "indexed_at" {
		t.Fatalf("unchanged metadata rewrites = %q, want only indexed_at", spy.setMetaKeys)
	}
	// Three change probes plus the run's own freshness probe, which lists
	// Git-visible files to find semantic inputs Git does not track.
	if report.GitCommands > 4 || report.Checked != 0 {
		t.Fatalf("unchanged refresh probes=%d checked=%d, want at most four Git commands and no content checks", report.GitCommands, report.Checked)
	}
	if report.CountsCollected {
		t.Fatalf("counts unexpectedly collected: %#v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["counts"]; exists {
		t.Fatalf("omitted counts serialized as real totals: %s", encoded)
	}
}

type noStatusRepository struct {
	graph.IndexRepository
	reconciles int
}

func (r *noStatusRepository) Reconcile(ctx context.Context) error {
	r.reconciles++
	return r.IndexRepository.Reconcile(ctx)
}

func TestServiceRepositoryWithoutCleanProofReconciles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	write(t, filepath.Join(root, "README.md"), "# Sample\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	if _, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	fallback := &noStatusRepository{IndexRepository: repository}
	if _, err := indexer.NewService(fallback, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if fallback.reconciles != 1 {
		t.Fatalf("repository without clean proof reconciled %d times, want 1", fallback.reconciles)
	}
}

func TestServiceCorruptPriorGitEvidenceFailsClosedAndRepairs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "README.md"), "# Sample\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	service := indexer.NewService(repository, registry)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"git_dirty_paths", "git_untracked_paths"} {
		t.Run(key, func(t *testing.T) {
			if err := repository.SetMeta(ctx, key, "{malformed"); err != nil {
				t.Fatal(err)
			}
			spy := &indexRepositorySpy{IndexRepository: repository}
			report, err := indexer.NewService(spy, registry).Run(ctx, project, indexer.Options{ReportDetail: indexer.ReportWithoutCounts})
			if err != nil {
				t.Fatal(err)
			}
			if report.Checked != 1 || spy.reconcileCalls != 1 {
				t.Fatalf("corrupt %s did not use safe path: report=%#v reconciles=%d", key, report, spy.reconcileCalls)
			}
			repaired, err := repository.Meta(ctx, key)
			if err != nil || repaired != "[]" {
				t.Fatalf("repaired %s = %q, err=%v", key, repaired, err)
			}
		})
	}
}

func TestServiceRejectsRemoteIdentityChangeBeforeMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "remote", "add", "origin", "git@example.invalid:team/first.git")
	write(t, filepath.Join(root, "README.md"), "# Sample\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	if _, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "remote", "set-url", "origin", "git@example.invalid:team/longer-second.git")
	changed, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.NewService(repository, registry).Run(ctx, changed, indexer.Options{}); err == nil || !strings.Contains(err.Error(), "repository identity changed") {
		t.Fatalf("remote identity change error = %v", err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("identity mismatch mutated graph: before=%#v after=%#v", before, after)
	}
	forced, err := indexer.NewService(repository, registry).Run(ctx, changed, indexer.Options{Force: true})
	if err != nil {
		t.Fatalf("force identity migration: %v", err)
	}
	if forced.Rebuild != "repository identity changed" || len(forced.Updated) != 1 {
		t.Fatalf("force identity migration report = %#v", forced)
	}
	storedID, err := repository.Meta(ctx, "repository_id")
	if err != nil || storedID != changed.ID {
		t.Fatalf("migrated repository ID = %q, want %q, err=%v", storedID, changed.ID, err)
	}
}

func TestServiceTrackedTypechangeUsesSafeSymlinkDiscovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "README.md"), "# In repository\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	service := indexer.NewService(repository, registry)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(testtemp.Dir(t), "outside.md")
	write(t, external, "# Outside secret\n")
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Skipped, []string{"README.md"}) || !reflect.DeepEqual(report.Removed, []string{"README.md"}) {
		t.Fatalf("tracked symlink report = %#v", report)
	}
	if report.Counts.Files != 0 {
		t.Fatalf("tracked symlink content was indexed: %#v", report.Counts)
	}
}

func TestServiceGitSnapshotConvergesAcrossMembershipAndHeadChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "first.md"), "# First\n")
	write(t, filepath.Join(root, "second.md"), "# Second\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(root, "first.md"), "# First staged\n")
	runGit(t, root, "add", "first.md")
	staged, err := service.Run(ctx, project, indexer.Options{})
	if err != nil || len(staged.Updated) != 1 || staged.Updated[0] != "first.md" {
		t.Fatalf("staged edit: report=%#v err=%v", staged, err)
	}

	runGit(t, root, "mv", "second.md", "renamed.md")
	renamed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil || len(renamed.Updated) != 1 || renamed.Updated[0] != "renamed.md" || len(renamed.Removed) != 1 || renamed.Removed[0] != "second.md" {
		t.Fatalf("rename: report=%#v err=%v", renamed, err)
	}

	write(t, filepath.Join(root, "untracked space.md"), "# Untracked\n")
	untracked, err := service.Run(ctx, project, indexer.Options{})
	if err != nil || len(untracked.Updated) != 1 || untracked.Updated[0] != "untracked space.md" {
		t.Fatalf("untracked path: report=%#v err=%v", untracked, err)
	}
	write(t, filepath.Join(root, "ignored-later.md"), "# Ignored later\n")
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 || report.Updated[0] != "ignored-later.md" {
		t.Fatalf("second untracked path: report=%#v err=%v", report, err)
	}
	exclude := filepath.Join(root, ".git", "info", "exclude")
	content, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, append(content, []byte("\nignored-later.md\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	ignored, err := service.Run(ctx, project, indexer.Options{})
	if err != nil || len(ignored.Removed) != 1 || ignored.Removed[0] != "ignored-later.md" {
		t.Fatalf("newly ignored path: report=%#v err=%v", ignored, err)
	}

	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "changes")
	committed, err := service.Run(ctx, project, indexer.Options{})
	// Three change probes plus the run's own freshness probe, which here spends
	// two: one listing Git-visible files for hidden semantic inputs and one
	// discovering workspace membership, since no earlier probe's keys are reused.
	if err != nil || committed.GitCommands > 5 {
		t.Fatalf("advanced HEAD: report=%#v err=%v", committed, err)
	}
	indexedCommit, err := repository.Meta(ctx, "commit")
	if err != nil || indexedCommit == "" || indexedCommit == project.Commit {
		t.Fatalf("indexed commit did not advance: commit=%q err=%v", indexedCommit, err)
	}
}

func TestServiceMissingWorkspaceDigestUsesSafePathOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	if _, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "workspace_state_digest", "malformed"); err != nil {
		t.Fatal(err)
	}
	spy := &indexRepositorySpy{IndexRepository: repository}
	if _, err := indexer.NewService(spy, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if spy.replaceOwnerCalls != 1 || spy.reconcileCalls != 1 {
		t.Fatalf("safe digest repair calls owner=%d reconcile=%d", spy.replaceOwnerCalls, spy.reconcileCalls)
	}
	spy.replaceOwnerCalls = 0
	if _, err := indexer.NewService(spy, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if spy.replaceOwnerCalls != 0 {
		t.Fatalf("valid digest did not enable unchanged path: %d owner replacements", spy.replaceOwnerCalls)
	}
}
