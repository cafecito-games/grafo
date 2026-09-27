package markdown_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
)

func TestParserExtractsHeadingHierarchyAndReferences(t *testing.T) {
	content := []byte(`# Architecture
See [the service](../internal/service.go) and [details](guide.md#Deep-Dive).

## Runtime
The function ` + "`Run`" + `, class ` + "`Checkout`" + `, and endpoint ` + "`POST /checkout`" + ` form the flow.

### Details
The method ` + "`Service.Start`" + ` begins it.

## Runtime
![diagram](diagram.png)
[website](https://example.com)

~~~md
# Hidden
function ` + "`Ignored`" + `
~~~
`)
	input := parserapi.Input{Path: "docs/ARCHITECTURE.md", Content: content, Repository: "sample", RepoID: "repo:sample"}
	result, err := markdownparser.New().Parse(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := markdownparser.New().Parse(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, again) {
		t.Fatal("same Markdown produced a different graph")
	}

	architecture := nodeByQualified(t, result.Nodes, "docs/ARCHITECTURE.md#architecture")
	runtime := nodeByQualified(t, result.Nodes, "docs/ARCHITECTURE.md#runtime")
	details := nodeByQualified(t, result.Nodes, "docs/ARCHITECTURE.md#details")
	duplicate := nodeByQualified(t, result.Nodes, "docs/ARCHITECTURE.md#runtime-1")
	if architecture.Kind != graph.KindDocSection || architecture.Location.EndLine <= duplicate.Location.Line {
		t.Fatalf("unexpected architecture section: %#v", architecture)
	}
	if runtime.Location.Line != 4 || runtime.Location.EndLine != 9 || details.Location.EndLine != 9 || duplicate.Location.Line != 10 {
		t.Fatalf("unexpected section spans: runtime=%#v details=%#v duplicate=%#v", runtime.Location, details.Location, duplicate.Location)
	}
	assertFact(t, result.Facts, graph.EdgeContains, architecture.ID, runtime.ID, "", graph.KindDocSection)
	assertFact(t, result.Facts, graph.EdgeContains, runtime.ID, details.ID, "", graph.KindDocSection)
	assertFact(t, result.Facts, graph.EdgeDocuments, architecture.ID, "", "internal/service.go", graph.KindFile)
	assertFact(t, result.Facts, graph.EdgeDocuments, architecture.ID, "", "docs/guide.md#deep-dive", graph.KindDocSection)
	assertFact(t, result.Facts, graph.EdgeDocuments, runtime.ID, "", "Run", graph.KindFunction)
	assertFact(t, result.Facts, graph.EdgeDocuments, runtime.ID, "", "Checkout", graph.KindClass)
	assertFact(t, result.Facts, graph.EdgeDocuments, runtime.ID, "", "POST /checkout", graph.KindEndpoint)
	assertFact(t, result.Facts, graph.EdgeDocuments, details.ID, "", "Service.Start", graph.KindMethod)
	assertNoTarget(t, result.Facts, "diagram.png")
	assertNoTarget(t, result.Facts, "https://example.com")
	assertNoTarget(t, result.Facts, "Ignored")
	for _, node := range result.Nodes {
		if node.Name == "Hidden" {
			t.Fatalf("fenced heading was indexed: %#v", node)
		}
	}
}

func TestParserRejectsMalformedHeadingsAndEscapingLinks(t *testing.T) {
	content := []byte("#valid-without-space\n####### too deep\n# Valid\n[escape](../../outside.go)\n[local](#Valid)\n")
	result, err := markdownparser.New().Parse(context.Background(), parserapi.Input{
		Path: "docs/readme.md", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodesOfKind(result.Nodes, graph.KindDocSection)) != 1 {
		t.Fatalf("malformed headings were indexed: %#v", result.Nodes)
	}
	assertNoTarget(t, result.Facts, "../outside.go")
	valid := nodeByQualified(t, result.Nodes, "docs/readme.md#valid")
	assertFact(t, result.Facts, graph.EdgeDocuments, valid.ID, "", "docs/readme.md#valid", graph.KindDocSection)
}

func TestParserSupportsSetextHeadingsAndReferenceLinks(t *testing.T) {
	content := []byte("Overview\n========\n\nSee [the worker][implementation].\n\n[implementation]: <../cmd/my%20worker.go> \"Worker source\"\n")
	result, err := markdownparser.New().Parse(context.Background(), parserapi.Input{
		Path: "docs/README.markdown", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	overview := nodeByQualified(t, result.Nodes, "docs/README.markdown#overview")
	if overview.Location.Line != 1 || overview.Properties["level"] != "1" {
		t.Fatalf("unexpected Setext heading: %#v", overview)
	}
	assertFact(t, result.Facts, graph.EdgeDocuments, overview.ID, "", "cmd/my worker.go", graph.KindFile)
}

func nodeByQualified(t *testing.T, nodes []graph.Node, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing node %q in %#v", qualified, nodes)
	return graph.Node{}
}

func nodesOfKind(nodes []graph.Node, kind graph.NodeKind) []graph.Node {
	var result []graph.Node
	for _, node := range nodes {
		if node.Kind == kind {
			result = append(result, node)
		}
	}
	return result
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, fromID, targetID, target string, targetKind graph.NodeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID && fact.TargetID == targetID && fact.Target == target && fact.TargetKind == targetKind {
			return
		}
	}
	t.Fatalf("missing %s fact from=%q target_id=%q target=%q target_kind=%q in %#v", kind, fromID, targetID, target, targetKind, facts)
}

func assertNoTarget(t *testing.T, facts []graph.Fact, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Target == target {
			t.Fatalf("unexpected fact target %q: %#v", target, fact)
		}
	}
}
