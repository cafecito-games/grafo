package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryResolvesAndInvalidatesNamedSources(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	handler := graph.Node{ID: "handler", Kind: graph.KindMethod, Name: "on_ready",
		QualifiedName: "Kit.on_ready", OwnerFile: "kit.gd"}
	fact := graph.Fact{ID: "handled", Source: "Backend.ready", SourceKind: graph.KindEvent,
		Kind: graph.EdgeHandledBy, TargetID: handler.ID, OwnerFile: "kit.gd"}
	if err := repository.ReplaceOwner(ctx, "kit.gd", graph.ParseResult{
		Nodes: []graph.Node{handler}, Facts: []graph.Fact{fact},
	}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, "Backend.ready", true)
	initialEdges, err := repository.EdgesTo(ctx, handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialEdgeID := initialEdges[0].ID
	assertExternalCount(t, ctx, repository, 1)

	event := graph.Node{ID: "event", Kind: graph.KindEvent, Name: "ready",
		QualifiedName: "Backend.ready", OwnerFile: "backend.gd"}
	if err := repository.ReplaceOwner(ctx, "backend.gd", graph.ParseResult{Nodes: []graph.Node{event}}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, event.ID, false)
	resolvedEdges, err := repository.EdgesTo(ctx, handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedEdges[0].ID != initialEdgeID {
		t.Fatalf("source resolution changed edge identity from %q to %q", initialEdgeID, resolvedEdges[0].ID)
	}
	assertExternalCount(t, ctx, repository, 0)

	// The explicit source kind filters a same-named declaration of another kind.
	field := graph.Node{ID: "field", Kind: graph.KindField, Name: "ready",
		QualifiedName: "Backend.ready", OwnerFile: "field.gd"}
	if err := repository.ReplaceOwner(ctx, "field.gd", graph.ParseResult{Nodes: []graph.Node{field}}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, event.ID, false)

	duplicate := graph.Node{ID: "event-two", Kind: graph.KindEvent, Name: "ready",
		QualifiedName: "Backend.ready", OwnerFile: "duplicate.gd"}
	if err := repository.ReplaceOwner(ctx, "duplicate.gd", graph.ParseResult{Nodes: []graph.Node{duplicate}}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, "Backend.ready", true)
	assertExternalCount(t, ctx, repository, 1)

	if err := repository.ReplaceOwner(ctx, "duplicate.gd", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, event.ID, false)
	assertExternalCount(t, ctx, repository, 0)

	renamed := event
	renamed.Name = "started"
	renamed.QualifiedName = "Backend.started"
	if err := repository.ReplaceOwner(ctx, "backend.gd", graph.ParseResult{Nodes: []graph.Node{renamed}}); err != nil {
		t.Fatal(err)
	}
	assertInboundSource(t, ctx, repository, handler.ID, "Backend.ready", true)
}

func TestRepositoryKeepsIndependentUnresolvedEndpointsExplicit(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	facts := []graph.Fact{
		{ID: "missing-exact", FromID: "missing-id", Kind: graph.EdgeCalls,
			Target: "Missing.target", TargetKind: graph.KindMethod, OwnerFile: "facts.gd"},
		{ID: "both-symbolic", Source: "Missing.signal", SourceKind: graph.KindEvent,
			Kind: graph.EdgeHandledBy, Target: "Missing.handler", TargetKind: graph.KindMethod, OwnerFile: "facts.gd"},
	}
	if err := repository.ReplaceOwner(ctx, "facts.gd", graph.ParseResult{Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Edges != 2 || counts.External != 4 {
		t.Fatalf("unresolved endpoint counts = %#v", counts)
	}
	exactSource, err := repository.Node(ctx, graph.NodeID(graph.KindExternal, "external:missing-id"))
	if err != nil || !exactSource.External || exactSource.QualifiedName != "missing-id" {
		t.Fatalf("missing exact source = %#v, err=%v", exactSource, err)
	}
}

func TestRepositoryReconcilesExactAndKindlessNamedSources(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	target := graph.Node{ID: "target", Kind: graph.KindMethod, Name: "target",
		QualifiedName: "target", OwnerFile: "facts.go"}
	facts := []graph.Fact{
		{ID: "exact", FromID: "exact-source", Kind: graph.EdgeCalls, TargetID: target.ID, OwnerFile: "facts.go"},
		{ID: "kindless", Source: "Shared.source", Kind: graph.EdgeCalls, TargetID: target.ID, OwnerFile: "facts.go"},
	}
	if err := repository.ReplaceOwner(ctx, "facts.go", graph.ParseResult{Nodes: []graph.Node{target}, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	exact := graph.Node{ID: "exact-source", Kind: graph.KindFunction, Name: "exact",
		QualifiedName: "Exact.source", OwnerFile: "exact.go"}
	event := graph.Node{ID: "shared-event", Kind: graph.KindEvent, Name: "source",
		QualifiedName: "Shared.source", OwnerFile: "event.go"}
	field := graph.Node{ID: "shared-field", Kind: graph.KindField, Name: "source",
		QualifiedName: "Shared.source", OwnerFile: "field.go"}
	if err := repository.ReplaceOwner(ctx, "exact.go", graph.ParseResult{Nodes: []graph.Node{exact}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "event.go", graph.ParseResult{Nodes: []graph.Node{event}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "field.go", graph.ParseResult{Nodes: []graph.Node{field}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesTo(ctx, target.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("edges = %#v, err=%v", edges, err)
	}
	assertFactSource(t, ctx, repository, edges, "exact", exact.ID, false)
	assertFactSource(t, ctx, repository, edges, "kindless", "Shared.source", true)

	// Replacing an exact source with the same ID cannot change the edge and
	// therefore must not manufacture reconciliation work.
	if err := repository.ReplaceOwner(ctx, "exact.go", graph.ParseResult{Nodes: []graph.Node{exact}}); err != nil {
		t.Fatal(err)
	}
	stats, err := repository.ReconcileWithStats(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Batches != 0 {
		t.Fatalf("unchanged exact source reconciled in %d batches", stats.Batches)
	}

	if err := repository.ReplaceOwner(ctx, "field.go", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err = repository.EdgesTo(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertFactSource(t, ctx, repository, edges, "kindless", event.ID, false)

	if err := repository.ReplaceOwner(ctx, "exact.go", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err = repository.EdgesTo(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertFactSource(t, ctx, repository, edges, "exact", "exact-source", true)
}

func TestRepositoryRejectsInvalidSourcesBeforeReplacingOwner(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	node := graph.Node{ID: "node", Kind: graph.KindFunction, Name: "node", QualifiedName: "node", OwnerFile: "owner.go"}
	valid := graph.Fact{ID: "self", FromID: node.ID, Kind: graph.EdgeCalls, TargetID: node.ID, OwnerFile: "owner.go"}
	if err := repository.ReplaceOwner(ctx, "owner.go", graph.ParseResult{Nodes: []graph.Node{node}, Facts: []graph.Fact{valid}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	invalid := []graph.Fact{
		{ID: "missing", Kind: graph.EdgeCalls, TargetID: node.ID},
		{ID: "both", FromID: node.ID, Source: "node", Kind: graph.EdgeCalls, TargetID: node.ID},
		{ID: "kind-only", FromID: node.ID, SourceKind: graph.KindEvent, Kind: graph.EdgeCalls, TargetID: node.ID},
	}
	for _, fact := range invalid {
		if err := repository.ReplaceOwner(ctx, "owner.go", graph.ParseResult{Facts: []graph.Fact{fact}}); err == nil {
			t.Fatalf("invalid source %#v was accepted", fact)
		}
		edges, err := repository.EdgesFrom(ctx, node.ID)
		if err != nil || len(edges) != 1 || edges[0].ToID != node.ID {
			t.Fatalf("failed replacement mutated prior graph: edges=%#v err=%v", edges, err)
		}
	}
}

func assertInboundSource(t *testing.T, ctx context.Context, repository *sqlite.Repository, targetID, want string, external bool) {
	t.Helper()
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesTo(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("inbound edges to %s = %#v", targetID, edges)
	}
	source, err := repository.Node(ctx, edges[0].FromID)
	if err != nil {
		t.Fatal(err)
	}
	if source.External != external {
		t.Fatalf("source %#v external = %v, want %v", source, source.External, external)
	}
	if external && source.QualifiedName != want {
		t.Fatalf("source qualified name = %q, want %q", source.QualifiedName, want)
	}
	if !external && source.ID != want {
		t.Fatalf("source ID = %q, want %q", source.ID, want)
	}
}

func assertFactSource(t *testing.T, ctx context.Context, repository *sqlite.Repository, edges []graph.Edge, factID, want string, external bool) {
	t.Helper()
	for _, edge := range edges {
		if edge.FactID != factID {
			continue
		}
		source, err := repository.Node(ctx, edge.FromID)
		if err != nil {
			t.Fatal(err)
		}
		if source.External != external {
			t.Fatalf("fact %s source %#v external = %v, want %v", factID, source, source.External, external)
		}
		if external && source.QualifiedName != want {
			t.Fatalf("fact %s source qualified name = %q, want %q", factID, source.QualifiedName, want)
		}
		if !external && source.ID != want {
			t.Fatalf("fact %s source ID = %q, want %q", factID, source.ID, want)
		}
		return
	}
	t.Fatalf("missing edge for fact %s in %#v", factID, edges)
}

func assertExternalCount(t *testing.T, ctx context.Context, repository *sqlite.Repository, want int) {
	t.Helper()
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.External != want {
		t.Fatalf("external nodes = %d, want %d", counts.External, want)
	}
}
