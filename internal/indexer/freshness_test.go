package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type freshnessParser struct{ semantic *string }

func (p freshnessParser) Language() string          { return "freshness" }
func (p freshnessParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (p freshnessParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}
func (p freshnessParser) WorkspaceSemanticKey(context.Context, parserapi.Input) (string, error) {
	if p.semantic == nil {
		return "", nil
	}
	return *p.semantic, nil
}

func TestFreshnessProbeDetectsSamePathSameSizeAndSemanticChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	path := filepath.Join(root, "sample.snap")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	semantic := "semantic-v1"
	registry := parserapi.NewRegistry(freshnessParser{semantic: &semantic})

	first, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Supported || first.Token.Version() == "" || !first.Token.Equal(second.Token) {
		t.Fatalf("stable Git probe = first %#v second %#v", first, second)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	dirty, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Token.Equal(dirty.Token) {
		t.Fatal("same-size and same-mtime dirty edit did not change freshness")
	}
	stableDirty, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil || !dirty.Token.Equal(stableDirty.Token) {
		t.Fatalf("unchanged dirty content was unstable: err=%v", err)
	}

	semantic = "semantic-v2"
	semanticChanged, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Token.Equal(semanticChanged.Token) {
		t.Fatal("workspace semantic key did not change freshness")
	}

	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"), []byte("unknown: one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configured, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if semanticChanged.Token.Equal(configured.Token) {
		t.Fatal("effective configuration input did not change freshness")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if configured.Token.Equal(deleted.Token) {
		t.Fatal("deleted tracked file did not change freshness")
	}
}

func TestFreshnessProbeFallsBackForNonGitAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	registry := parserapi.NewRegistry(freshnessParser{})
	probe, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Supported || probe.Fallback == "" {
		t.Fatalf("non-Git probe did not request conservative fallback: %#v", probe)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{}); err == nil {
		t.Fatal("canceled freshness probe succeeded")
	}
}

func TestFreshnessProbeRejectsOversizedDirtyInput(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	path := filepath.Join(root, "sample.snap")
	if err := os.WriteFile(path, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	if err := os.WriteFile(path, []byte("too-large"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe, err := indexer.ProbeFreshness(ctx, root, parserapi.NewRegistry(freshnessParser{}),
		indexer.FreshnessOptions{MaxFileSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !probe.Supported {
		t.Fatalf("bounded oversized state should remain deterministic: %#v", probe)
	}
}

func TestFreshnessProbeTracksBranchCommitStatusAndRemoteIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	path := filepath.Join(root, "sample.snap")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	runGit(t, root, "remote", "add", "origin", "https://example.invalid/a")
	registry := parserapi.NewRegistry(freshnessParser{})
	main, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}

	runGit(t, root, "checkout", "-b", "feature")
	feature, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if main.Token.Equal(feature.Token) || main.Project.IndexPath == feature.Project.IndexPath {
		t.Fatal("branch transition retained freshness or index identity")
	}
	if err := os.WriteFile(path, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	staged, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if feature.Token.Equal(staged.Token) {
		t.Fatal("staged edit retained freshness")
	}
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "changed")
	committed, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if staged.Token.Equal(committed.Token) {
		t.Fatal("commit transition retained freshness")
	}

	configPath := filepath.Join(root, ".git", "config")
	configInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "remote", "set-url", "origin", "https://example.invalid/b")
	if err := os.Chtimes(configPath, configInfo.ModTime(), configInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	identityChanged, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Token.Equal(identityChanged.Token) || committed.Project.ID == identityChanged.Project.ID {
		t.Fatal("same-size and same-mtime remote identity edit retained freshness")
	}
}

func TestFreshnessProbeIgnoresUnrelatedAndTracksRenameAndSymlinkStates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	registry := parserapi.NewRegistry(freshnessParser{})
	clean, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not parsed"), 0o644); err != nil {
		t.Fatal(err)
	}
	unrelated, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Token.Equal(unrelated.Token) {
		t.Fatal("unrelated untracked file invalidated source freshness")
	}
	runGit(t, root, "mv", "sample.snap", "renamed.snap")
	renamed, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Token.Equal(renamed.Token) {
		t.Fatal("rename retained freshness")
	}
	if err := os.Remove(filepath.Join(root, "renamed.snap")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(root, "renamed.snap")); err != nil {
		t.Fatal(err)
	}
	symlink, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Token.Equal(symlink.Token) {
		t.Fatal("symlink transition retained freshness")
	}
}
