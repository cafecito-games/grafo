package parser_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/parser"
)

func TestBuilderFileIDUsesRepositoryIdentity(t *testing.T) {
	input := parser.Input{Repository: "same-name", RepoID: "repo:first", Path: "main.go"}
	builder := parser.NewBuilder(input, "go")

	want := graph.NodeID(graph.KindFile, "repo:first:main.go")
	if builder.FileID() != want {
		t.Fatalf("file ID = %q, want %q", builder.FileID(), want)
	}
	if len(builder.Result.Nodes) != 1 || builder.Result.Nodes[0].ID != want {
		t.Fatalf("file node and builder disagree: %#v", builder.Result.Nodes)
	}

	other := parser.NewBuilder(parser.Input{Repository: "same-name", RepoID: "repo:second", Path: "main.go"}, "go")
	if other.FileID() == builder.FileID() {
		t.Fatal("different repository identities produced the same file ID")
	}
}

func TestBuilderDistinguishesExactAndNamedSourceIdentity(t *testing.T) {
	input := parser.Input{Repository: "sample", RepoID: "repo:sample", Path: "main.gd"}
	exact := parser.NewBuilder(input, "gdscript")
	exact.AddFact("Backend.ready", graph.EdgeHandledBy, "handler", "", graph.KindMethod,
		graph.Location{Path: input.Path, Line: 4}, nil)
	named := parser.NewBuilder(input, "gdscript")
	named.AddNamedSourceFact("Backend.ready", graph.KindEvent, graph.EdgeHandledBy,
		"handler", "", graph.KindMethod, graph.Location{Path: input.Path, Line: 4}, nil)

	if len(named.Result.Facts) != 1 {
		t.Fatalf("named facts = %#v", named.Result.Facts)
	}
	fact := named.Result.Facts[0]
	if fact.FromID != "" || fact.Source != "Backend.ready" || fact.SourceKind != graph.KindEvent {
		t.Fatalf("named source fact = %#v", fact)
	}
	if fact.ID == exact.Result.Facts[0].ID {
		t.Fatalf("exact and named source locators collided at %q", fact.ID)
	}
}
