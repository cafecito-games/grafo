package kvbench

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryMatchesSQLiteResolutionAndQueries(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			control, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close() }()
			candidate, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{ReconciliationBatchSize: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = candidate.Close() }()

			for _, repository := range []graph.Repository{control, candidate} {
				seedRepository(t, repository)
			}
			assertEquivalent(t, control, candidate)

			// Adding the previously missing declaration must move the unresolved
			// edge to the internal node and collect the orphan external node.
			missing := testNode("missing", graph.KindFunction, "pkg.Missing", "missing.go")
			for _, repository := range []graph.Repository{control, candidate} {
				if err := repository.ReplaceFile(ctx, testFile("missing.go"), graph.ParseResult{Nodes: []graph.Node{missing}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertEquivalent(t, control, candidate)
		})
	}
}

func TestRepositoryDirectTestEdgesMatchSQLite(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			control, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close() }()
			candidate, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = candidate.Close() }()

			testDeclaration := testNode("test", graph.KindTest, "pkg.TestProduce", "sample_test.go")
			target := testNode("target", graph.KindFunction, "pkg.Produce", "sample.go")
			fact := testFact("test-call", testDeclaration.ID, graph.EdgeCalls, target.QualifiedName, target.Kind, "", testDeclaration.OwnerFile)
			fact.Producer = "go"
			for _, repository := range []graph.Repository{control, candidate} {
				if err := repository.ReplaceFile(ctx, testFile(testDeclaration.OwnerFile), graph.ParseResult{Nodes: []graph.Node{testDeclaration}, Facts: []graph.Fact{fact}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReplaceFile(ctx, testFile(target.OwnerFile), graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			controlEdges, err := control.EdgesFrom(ctx, testDeclaration.ID)
			if err != nil {
				t.Fatal(err)
			}
			candidateEdges, err := candidate.EdgesFrom(ctx, testDeclaration.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(controlEdges, candidateEdges) {
				t.Fatalf("test edges differ:\ncontrol=%#v\ncandidate=%#v", controlEdges, candidateEdges)
			}
			if len(controlEdges) != 2 || controlEdges[1].Kind != graph.EdgeTests {
				t.Fatalf("direct test edge missing: %#v", controlEdges)
			}
		})
	}
}

func TestRepositoryExplicitTargetKindMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			control, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close() }()
			candidate, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = candidate.Close() }()

			source := testNode("source", graph.KindFunction, "pkg.Source", "source.go")
			target := testNode("target", graph.KindVariable, "pkg.Target", "target.go")
			fact := testFact("reference", source.ID, graph.EdgeReferences, target.QualifiedName, graph.KindVariable, "", "source.go")
			for _, repository := range []graph.Repository{control, candidate} {
				if err := repository.ReplaceFile(ctx, testFile("source.go"), graph.ParseResult{Nodes: []graph.Node{source}, Facts: []graph.Fact{fact}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReplaceFile(ctx, testFile("target.go"), graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertEquivalent(t, control, candidate)
		})
	}
}

func TestRepositoryKindlessGodotResolutionMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			control, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close() }()
			candidate, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = candidate.Close() }()

			source := testNode("a", graph.KindFunction, "pkg.A", "source.go")
			target := testNode("scene", graph.KindGodotScene, "scenes/main", "main.tscn")
			fact := testFact("reference", source.ID, graph.EdgeReferences, target.QualifiedName, "", "", "source.go")
			for _, repository := range []graph.Repository{control, candidate} {
				if err := repository.ReplaceFile(ctx, testFile("source.go"), graph.ParseResult{Nodes: []graph.Node{source}, Facts: []graph.Fact{fact}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReplaceFile(ctx, testFile("main.tscn"), graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
					t.Fatal(err)
				}
				if err := repository.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertEquivalent(t, control, candidate)
		})
	}
}

func TestRepositorySearchAndMatchMirrorSQLiteTextSemantics(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			control, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close() }()
			candidate, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = candidate.Close() }()

			nodes := []graph.Node{
				testNode("wildcard", graph.KindFunction, "pkg.aXb", "one.go"),
				{ID: "n:unicode", Kind: graph.KindFunction, Name: "needle", QualifiedName: "é", OwnerFile: "one.go"},
				{ID: "n:ascii", Kind: graph.KindFunction, Name: "needle", QualifiedName: "aa", OwnerFile: "one.go"},
				{ID: "n:accent", Kind: graph.KindFunction, Name: "café", QualifiedName: "pkg.café", OwnerFile: "one.go"},
			}
			for _, repository := range []graph.Repository{control, candidate} {
				if err := repository.ReplaceFile(ctx, testFile("one.go"), graph.ParseResult{Nodes: nodes}); err != nil {
					t.Fatal(err)
				}
			}
			for _, term := range []string{"a_b", "needle", "CAFÉ"} {
				controlNodes, err := control.SearchNodes(ctx, term, 100)
				if err != nil {
					t.Fatal(err)
				}
				candidateNodes, err := candidate.SearchNodes(ctx, term, 100)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(controlNodes, candidateNodes) {
					t.Fatalf("search %q differs:\ncontrol=%#v\ncandidate=%#v", term, controlNodes, candidateNodes)
				}
				controlMatch, err := control.MatchNodes(ctx, graph.NodeMatchQuery{Selector: term, Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				candidateMatch, err := candidate.MatchNodes(ctx, graph.NodeMatchQuery{Selector: term, Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(controlMatch, candidateMatch) {
					t.Fatalf("match %q differs:\ncontrol=%#v\ncandidate=%#v", term, controlMatch, candidateMatch)
				}
			}
		})
	}
}

func TestRepositoryRestartResumesCommittedReconciliationBatches(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate")
			repository, err := Open(ctx, engine, path, Options{ReconciliationBatchSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			seedRepositoryWithoutReconcile(t, repository)
			stop := errors.New("stop after durable batch")
			stats, err := repository.ReconcileWithStats(ctx, func(stats graph.ReconciliationStats) error {
				if stats.Batches == 1 {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) || stats.Batches != 1 {
				t.Fatalf("interruption = (%#v, %v), want one committed batch", stats, err)
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}

			repository, err = Open(ctx, engine, path, Options{ReconciliationBatchSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = repository.Close() }()
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			counts, err := repository.Counts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if counts.Facts != 4 || counts.Edges != 4 || counts.External != 2 {
				t.Fatalf("resumed counts = %#v", counts)
			}
		})
	}
}

func TestRepositoryPreservesProducerAcrossRestartAndReplacement(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate")
			repository, err := Open(ctx, engine, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			source := testNode("producer-source", graph.KindMethod, "Player.poll", "player.gd")
			target := testNode("producer-target", graph.KindGodotInputAction,
				"godot:input_action:project.godot:jump", "project.godot")
			fact := testFact("producer-fact", source.ID, graph.EdgeUsesInputAction,
				target.QualifiedName, target.Kind, "", "player.gd")
			fact.Producer = graph.ProducerGDScript
			if err := repository.ReplaceFile(ctx, testFile("player.gd"), graph.ParseResult{
				Nodes: []graph.Node{source}, Facts: []graph.Fact{fact},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReplaceFile(ctx, testFile("project.godot"), graph.ParseResult{
				Nodes: []graph.Node{target},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assertKVProducerEdge(t, ctx, repository, source.ID, graph.ProducerGDScript, false)
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}

			repository, err = Open(ctx, engine, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = repository.Close() }()
			assertKVProducerEdge(t, ctx, repository, source.ID, graph.ProducerGDScript, false)
			if err := repository.ReplaceFile(ctx, testFile("project.godot"), graph.ParseResult{}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assertKVProducerEdge(t, ctx, repository, source.ID, graph.ProducerGDScript, true)
			fact.Producer = graph.ProducerGodot
			if err := repository.ReplaceFile(ctx, testFile("player.gd"), graph.ParseResult{
				Nodes: []graph.Node{source}, Facts: []graph.Fact{fact},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assertKVProducerEdge(t, ctx, repository, source.ID, graph.ProducerGodot, true)
			if err := repository.ReplaceFile(ctx, testFile("project.godot"), graph.ParseResult{
				Nodes: []graph.Node{target},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assertKVProducerEdge(t, ctx, repository, source.ID, graph.ProducerGodot, false)
		})
	}
}

func assertKVProducerEdge(t *testing.T, ctx context.Context, repository *Repository,
	fromID, producer string, external bool) {
	t.Helper()
	edges, err := repository.EdgesFrom(ctx, fromID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Producer != producer {
		t.Fatalf("producer edges = %#v, want producer %q", edges, producer)
	}
	target, err := repository.Node(ctx, edges[0].ToID)
	if err != nil {
		t.Fatal(err)
	}
	if target.External != external {
		t.Fatalf("target external = %v, want %v: %#v", target.External, external, target)
	}
}

func TestRepositoryReplacementIsAtomicOnCanceledContext(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{EngineBolt, EnginePebble} {
		t.Run(string(engine), func(t *testing.T) {
			repository, err := Open(ctx, engine, filepath.Join(t.TempDir(), "candidate"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = repository.Close() }()
			node := testNode("original", graph.KindFunction, "pkg.Original", "one.go")
			if err := repository.ReplaceFile(ctx, testFile("one.go"), graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
				t.Fatal(err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			replacement := testNode("replacement", graph.KindFunction, "pkg.Replacement", "one.go")
			if err := repository.ReplaceFile(canceled, testFile("one.go"), graph.ParseResult{Nodes: []graph.Node{replacement}}); !errors.Is(err, context.Canceled) {
				t.Fatalf("replacement error = %v, want context cancellation", err)
			}
			if _, err := repository.Node(ctx, node.ID); err != nil {
				t.Fatalf("committed node lost after canceled replacement: %v", err)
			}
			if _, err := repository.Node(ctx, replacement.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("replacement node error = %v, want not found", err)
			}
		})
	}
}

func seedRepository(t *testing.T, repository graph.Repository) {
	t.Helper()
	seedRepositoryWithoutReconcile(t, repository)
	if err := repository.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func seedRepositoryWithoutReconcile(t *testing.T, repository graph.Repository) {
	t.Helper()
	a := testNode("a", graph.KindFunction, "pkg.A", "one.go")
	b1 := testNode("b1", graph.KindFunction, "pkg.Duplicate", "two.go")
	b2 := testNode("b2", graph.KindFunction, "other.Duplicate", "two.go")
	facts := []graph.Fact{
		testFact("direct", a.ID, graph.EdgeCalls, "", "", a.ID, "one.go"),
		testFact("qualified", a.ID, graph.EdgeCalls, "pkg.Duplicate", graph.KindFunction, "", "one.go"),
		testFact("ambiguous", a.ID, graph.EdgeCalls, "Duplicate", graph.KindFunction, "", "one.go"),
		testFact("missing", a.ID, graph.EdgeCalls, "pkg.Missing", graph.KindFunction, "", "one.go"),
	}
	if err := repository.ReplaceFile(context.Background(), testFile("one.go"), graph.ParseResult{Nodes: []graph.Node{a}, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceFile(context.Background(), testFile("two.go"), graph.ParseResult{Nodes: []graph.Node{b2, b1}}); err != nil {
		t.Fatal(err)
	}
}

func assertEquivalent(t *testing.T, control, candidate graph.Repository) {
	t.Helper()
	ctx := context.Background()
	controlCounts, err := control.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidateCounts, err := candidate.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controlCounts, candidateCounts) {
		t.Fatalf("counts differ:\ncontrol=%#v\ncandidate=%#v", controlCounts, candidateCounts)
	}
	for _, term := range []string{"A", "Duplicate", "Missing"} {
		controlNodes, err := control.SearchNodes(ctx, term, 100)
		if err != nil {
			t.Fatal(err)
		}
		candidateNodes, err := candidate.SearchNodes(ctx, term, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(controlNodes, candidateNodes) {
			t.Fatalf("search %q differs:\ncontrol=%#v\ncandidate=%#v", term, controlNodes, candidateNodes)
		}
	}
	for _, request := range []graph.NodeMatchQuery{
		{Selector: "pkg.A"},
		{Selector: "Duplicate", Kind: graph.KindFunction},
		{Selector: "missing", Limit: 1},
	} {
		controlMatch, err := control.MatchNodes(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		candidateMatch, err := candidate.MatchNodes(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(controlMatch, candidateMatch) {
			t.Fatalf("match %#v differs:\ncontrol=%#v\ncandidate=%#v", request, controlMatch, candidateMatch)
		}
	}
	a := testNode("a", graph.KindFunction, "pkg.A", "one.go")
	controlEdges, err := control.EdgesFrom(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidateEdges, err := candidate.EdgesFrom(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(controlEdges, func(i, j int) bool { return controlEdges[i].ID < controlEdges[j].ID })
	sort.Slice(candidateEdges, func(i, j int) bool { return candidateEdges[i].ID < candidateEdges[j].ID })
	if !reflect.DeepEqual(controlEdges, candidateEdges) {
		t.Fatalf("outgoing edges differ:\ncontrol=%#v\ncandidate=%#v", controlEdges, candidateEdges)
	}
}

func testFile(path string) graph.FileRecord {
	return graph.FileRecord{Path: path, Hash: "hash-" + path, Language: "go", Size: 10, ModifiedNS: 1, IndexedAt: "2026-01-01T00:00:00Z"}
}

func testNode(id string, kind graph.NodeKind, qualified, owner string) graph.Node {
	return graph.Node{ID: "n:" + id, Kind: kind, Name: graph.SimpleName(qualified), QualifiedName: qualified,
		Language: "go", Location: graph.Location{Path: owner, Line: 1}, Properties: map[string]string{"fixture": "true"}, OwnerFile: owner}
}

func testFact(id, from string, kind graph.EdgeKind, target string, targetKind graph.NodeKind, targetID, owner string) graph.Fact {
	return graph.Fact{ID: "f:" + id, FromID: from, Kind: kind, Target: target, TargetKind: targetKind, TargetID: targetID,
		Location: graph.Location{Path: owner, Line: 2}, Properties: map[string]string{"fixture": "true"}, OwnerFile: owner}
}
