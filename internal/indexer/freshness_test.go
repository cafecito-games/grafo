package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	"github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type freshnessParser struct{ semantic *string }

func (p freshnessParser) Language() string          { return "freshness" }
func (p freshnessParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (p freshnessParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}

type cacheableFreshnessParser struct {
	evidence *string
	calls    *int
}

func (cacheableFreshnessParser) Language() string          { return "cacheable" }
func (cacheableFreshnessParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (cacheableFreshnessParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}
func (p cacheableFreshnessParser) WorkspaceSemanticKey(context.Context, parserapi.Input) (string, error) {
	*p.calls++
	return "workspace-key:" + *p.evidence, nil
}
func (p cacheableFreshnessParser) WorkspaceSemanticEvidenceKey(context.Context, parserapi.Input) (string, error) {
	return *p.evidence, nil
}
func (cacheableFreshnessParser) SemanticDependencies() []string { return []string{"semantic.cfg"} }
func (p freshnessParser) WorkspaceSemanticKey(context.Context, parserapi.Input) (string, error) {
	if p.semantic == nil {
		return "", nil
	}
	return *p.semantic, nil
}

type projectFreshnessParser struct{}

func (projectFreshnessParser) Language() string          { return "project-freshness" }
func (projectFreshnessParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (projectFreshnessParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}
func (projectFreshnessParser) SemanticDependencies() []string { return []string{"project.godot"} }

type scopedFreshnessParser struct{ paths *[]string }

func (scopedFreshnessParser) Language() string          { return "scoped-freshness" }
func (scopedFreshnessParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (scopedFreshnessParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}
func (p scopedFreshnessParser) WorkspaceSemanticKey(_ context.Context, input parserapi.Input) (string, error) {
	*p.paths = append([]string(nil), input.SourcePaths...)
	return strings.Join(input.SourcePaths, "\x00"), nil
}
func (scopedFreshnessParser) SemanticDependencies() []string { return []string{"project.godot"} }

func TestFreshnessHonorsConfiguredIndexScope(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	for path, content := range map[string]string{
		"grafo.yaml":             "index:\n  exclude:\n    - excluded/**\n",
		"sample.snap":            "included",
		"excluded/hidden.snap":   "excluded",
		"excluded/project.godot": "[application]\nconfig/name=\"excluded\"\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	var paths []string
	registry := parserapi.NewRegistry(scopedFreshnessParser{paths: &paths})
	first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "sample.snap" {
		t.Fatalf("workspace semantic membership = %v, want only included source", paths)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded", "project.godot"), []byte("[application]\nconfig/name=\"changed\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Token.Equal(second.Token) {
		t.Fatal("configured-out semantic dependency changed freshness")
	}
}

func TestFreshnessIgnoresProjectsOutsideGitMembership(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n.godot/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	for _, path := range []string{"ignored/project.godot", ".godot/project.godot"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("small"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := parserapi.NewRegistry(projectFreshnessParser{})
	first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{MaxFileSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Supported {
		t.Fatalf("initial probe unsupported: %#v", first)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "project.godot"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{MaxFileSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Token.Equal(second.Token) {
		t.Fatal("Git-ignored project.godot changed freshness")
	}
	if err := os.WriteFile(filepath.Join(root, ".godot", "project.godot"), []byte(strings.Repeat("x", 128)), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := indexer.ReprobeFreshness(context.Background(), second, registry, indexer.FreshnessOptions{MaxFileSize: 64})
	if err != nil {
		t.Fatalf("oversized excluded project.godot failed freshness: %v", err)
	}
	if !second.Token.Equal(third.Token) {
		t.Fatal("hard-excluded project.godot changed freshness")
	}
}

func TestFreshnessProbeDetectsSamePathSameSizeAndSemanticChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
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
	t.Parallel()
	root := testtemp.Dir(t)
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

func TestFreshnessReprobeCachesKeysOnlyWhileExternalEvidenceAndGitInputsMatch(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "sample.snap")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	evidence, calls := "external-v1", 0
	registry := parserapi.NewRegistry(cacheableFreshnessParser{evidence: &evidence, calls: &calls})
	first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !first.Token.Equal(second.Token) {
		t.Fatalf("clean reprobe calls=%d equal=%t", calls, first.Token.Equal(second.Token))
	}
	semanticPath := filepath.Join(root, "semantic.cfg")
	if err := os.WriteFile(semanticPath, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	semanticChanged, err := indexer.ReprobeFreshness(context.Background(), second, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || second.Token.Equal(semanticChanged.Token) {
		t.Fatalf("semantic dependency change calls=%d equal=%t", calls, second.Token.Equal(semanticChanged.Token))
	}
	info, err := os.Stat(semanticPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(semanticPath, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(semanticPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	repeatedDependencyEdit, err := indexer.ReprobeFreshness(context.Background(), semanticChanged, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || semanticChanged.Token.Equal(repeatedDependencyEdit.Token) {
		t.Fatalf("repeated dependency edit calls=%d equal=%t", calls, semanticChanged.Token.Equal(repeatedDependencyEdit.Token))
	}
	evidence = "external-v2"
	third, err := indexer.ReprobeFreshness(context.Background(), repeatedDependencyEdit, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || repeatedDependencyEdit.Token.Equal(third.Token) {
		t.Fatalf("external evidence change calls=%d equal=%t", calls, repeatedDependencyEdit.Token.Equal(third.Token))
	}
}

func TestFreshnessProbeRejectsOversizedDirtyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
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

func TestFreshnessReprobeHashesRepeatedTypeScriptSemanticInput(t *testing.T) {
	t.Parallel()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.ts"), []byte("export const value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte(`{"strict":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	registry := parserapi.NewRegistry(typescript.New())
	first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tsconfig.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"strict":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Token.Equal(second.Token) {
		t.Fatal("TypeScript semantic input edit retained freshness")
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"strict":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, secondInfo.ModTime(), secondInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	third, err := indexer.ReprobeFreshness(context.Background(), second, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Token.Equal(third.Token) {
		t.Fatal("same-path same-mtime TypeScript semantic input edit retained freshness")
	}
}

func TestFreshnessReprobeDetectsIgnoredVendorSemanticInputs(t *testing.T) {
	t.Parallel()
	for _, vendorPath := range []string{"vendor", "cmd/vendor"} {
		t.Run(vendorPath, func(t *testing.T) {
			root := testtemp.Dir(t)
			runGit(t, root, "init", "-b", "main")
			files := map[string]string{
				".gitignore": "vendor/\n",
				"go.mod":     "module example.invalid/app\n\ngo 1.26\n",
				"main.go":    "package main\nfunc main() {}\n",
			}
			for path, content := range files {
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, root, "add", ".")
			runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
			registry := parserapi.NewRegistry(golangparser.New())
			first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
			if err != nil {
				t.Fatal(err)
			}
			vendorPackage := filepath.Join(root, vendorPath, "example.invalid", "lib")
			if err := os.MkdirAll(vendorPackage, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, vendorPath, "modules.txt"), []byte("# example.invalid/lib v1.0.0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(vendorPackage, "lib.go"), []byte("package lib\nconst Value = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			reprobe, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{})
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if first.Token.Equal(reprobe.Token) {
				t.Fatal("ignored vendor semantic inputs retained the published token")
			}
			if !reprobe.Token.Equal(fresh.Token) {
				t.Fatal("reprobe and full probe disagree after ignored vendor semantic inputs")
			}
		})
	}
}

func TestFreshnessReprobeIgnoresRootModulesTxtInVendorNamedCheckout(t *testing.T) {
	t.Parallel()
	root := filepath.Join(testtemp.Dir(t), "vendor")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "main")
	files := map[string]string{
		".gitignore": "modules.txt\n",
		"go.mod":     "module example.invalid/app\n\ngo 1.26\n",
		"main.go":    "package main\nfunc main() {}\n",
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	registry := parserapi.NewRegistry(golangparser.New())
	first, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "modules.txt"), []byte("not a vendored module manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reprobe, err := indexer.ReprobeFreshness(context.Background(), first, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := indexer.ProbeFreshness(context.Background(), root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Token.Equal(reprobe.Token) || !reprobe.Token.Equal(fresh.Token) {
		t.Fatal("root-relative modules.txt classification disagrees between full and repeated probes")
	}
}

func TestFreshnessProbeTracksBranchCommitStatusAndRemoteIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
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
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
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

func newRepository(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.name", "Grafo Test")
	runGit(t, root, "config", "user.email", "grafo@example.invalid")
	return root
}

func indexProject(t *testing.T, ctx context.Context, root string) indexer.Project {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)
	return project
}

func metaValue(t *testing.T, ctx context.Context, indexPath, key string) string {
	t.Helper()
	repository, err := sqlite.OpenReadOnly(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	value, err := repository.Meta(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// TestFreshnessTokenIsStoredByASuccessfulRun is the whole contract of the
// persisted token: a run records the inputs it indexed, so a later process can
// compare without indexing, and an edit after the run breaks the comparison.
func TestFreshnessTokenIsStoredByASuccessfulRun(t *testing.T) {
	ctx := context.Background()
	root := newRepository(t)
	write(t, filepath.Join(root, "pkg", "pkg.go"), "package pkg\n\nfunc Exported() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")

	project := indexProject(t, ctx, root)

	stored := metaValue(t, ctx, project.IndexPath, indexer.FreshnessTokenMeta)
	if stored == "" {
		t.Fatal("a successful run stored no freshness token")
	}

	probe, err := indexer.ProbeFreshness(ctx, root, parserdefaults.NewRegistry(), indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Token.String() != stored {
		t.Fatalf("stored token = %q, probe token = %q", stored, probe.Token.String())
	}

	// An edit after the run must break the comparison, or a query would be
	// served from an index that does not describe the working tree.
	write(t, filepath.Join(root, "pkg", "pkg.go"), "package pkg\n\nfunc Exported() {}\n\nfunc Added() {}\n")
	after, err := indexer.ProbeFreshness(ctx, root, parserdefaults.NewRegistry(), indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Token.String() == stored {
		t.Fatal("an edited working tree still matched the stored freshness token")
	}
}

// TestFreshnessTokenIsClearedForANonGitProject covers the conservative case:
// a project Git cannot describe must never be served without a refresh, so no
// token may remain to prove it fresh.
func TestFreshnessTokenIsClearedForANonGitProject(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	write(t, filepath.Join(root, "pkg", "pkg.go"), "package pkg\n\nfunc Exported() {}\n")

	project := indexProject(t, ctx, root)

	if stored := metaValue(t, ctx, project.IndexPath, indexer.FreshnessTokenMeta); stored != "" {
		t.Fatalf("a non-Git project stored freshness token %q", stored)
	}
}

// TestFreshnessTokenFromOptionsIsStoredWithoutASecondProbe pins that a caller
// which already probed does not pay for probing twice.
func TestFreshnessTokenFromOptionsIsStoredWithoutASecondProbe(t *testing.T) {
	ctx := context.Background()
	root := newRepository(t)
	write(t, filepath.Join(root, "pkg", "pkg.go"), "package pkg\n\nfunc Exported() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")

	registry := parserdefaults.NewRegistry()
	probe, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}

	repository, err := sqlite.Open(ctx, probe.Project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.NewService(repository, registry).Run(ctx, probe.Project,
		indexer.Options{Token: probe.Token}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	if stored := metaValue(t, ctx, probe.Project.IndexPath, indexer.FreshnessTokenMeta); stored != probe.Token.String() {
		t.Fatalf("stored token = %q, want the supplied %q", stored, probe.Token.String())
	}
}
