package semantic_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type fakeEmbedder struct {
	calls int
}

func (*fakeEmbedder) Model() string { return "test-model" }

func (e *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	result := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		switch {
		case strings.Contains(strings.ToLower(input), "charge") || strings.Contains(strings.ToLower(input), "payment"):
			result = append(result, []float32{1, 0})
		case strings.Contains(strings.ToLower(input), "refund"):
			result = append(result, []float32{0.8, 0.2})
		default:
			result = append(result, []float32{0, 1})
		}
	}
	return result, nil
}

func TestServiceIncrementallySyncsAndReturnsGraphContext(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	charge := graph.Node{ID: "charge", Kind: graph.KindFunction, Name: "ChargeCard",
		QualifiedName: "payments.ChargeCard", OwnerFile: "payments.go", Properties: map[string]string{"signature": "func(amount int) error"}}
	email := graph.Node{ID: "email", Kind: graph.KindFunction, Name: "SendEmail",
		QualifiedName: "notifications.SendEmail", OwnerFile: "email.go"}
	if err := repository.ReplaceOwner(ctx, "payments.go", graph.ParseResult{Nodes: []graph.Node{charge}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "email.go", graph.ParseResult{Nodes: []graph.Node{email}, Facts: []graph.Fact{{
		ID: "calls-charge", FromID: email.ID, Kind: graph.EdgeCalls, TargetID: charge.ID, OwnerFile: "email.go",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	embedder := &fakeEmbedder{}
	service := semantic.NewService(repository, repository, embedder).WithBatchSize(1)
	first, err := service.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Updated != 2 || first.Unchanged != 0 || embedder.calls != 2 {
		t.Fatalf("unexpected first sync: %#v, calls=%d", first, embedder.calls)
	}
	second, err := service.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Updated != 0 || second.Unchanged != 2 || embedder.calls != 2 {
		t.Fatalf("unchanged candidates were embedded again: %#v, calls=%d", second, embedder.calls)
	}

	result, err := service.Search(ctx, "take a payment", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Node.ID != charge.ID {
		t.Fatalf("unexpected semantic match: %#v", result.Matches)
	}
	if len(result.Matches[0].Context.Edges) != 1 || result.Matches[0].Context.Edges[0].Kind != graph.EdgeCalls {
		t.Fatalf("semantic candidate lacks structural context: %#v", result.Matches[0].Context)
	}

	if err := repository.ReplaceOwner(ctx, "email.go", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	third, err := service.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Removed != 1 || third.Unchanged != 1 {
		t.Fatalf("stale embedding was not removed: %#v", third)
	}
}

func TestDocumentIsStableAndHumanizesIdentifiers(t *testing.T) {
	node := graph.Node{Kind: graph.KindFunction, Name: "FindReusableCode", QualifiedName: "search.FindReusableCode",
		Language: "go", Properties: map[string]string{"signature": "func(query string)"}}
	want := "function\nFind Reusable Code\nsearch Find Reusable Code\nlanguage go\nsignature func query string"
	if got := semantic.Document(node); got != want {
		t.Fatalf("document = %q, want %q", got, want)
	}
}
