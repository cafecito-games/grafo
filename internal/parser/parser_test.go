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
