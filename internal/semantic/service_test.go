package semantic_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	embeddingcache "github.com/cafecito-games/grafo/internal/embedding/cache"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type fakeEmbedder struct {
	calls      int
	inputs     int
	dimensions int
	err        error
	failCall   int
	malformed  string
}

func (*fakeEmbedder) Model() string { return "test-model" }

func (e *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	e.inputs += len(inputs)
	if e.err != nil && (e.failCall == 0 || e.calls == e.failCall) {
		return nil, e.err
	}
	result := make([][]float32, 0, len(inputs))
	for index, input := range inputs {
		if e.malformed == "count" && index == len(inputs)-1 {
			continue
		}
		if e.malformed == "mixed" && index == len(inputs)-1 {
			result = append(result, []float32{1, 0, 0})
			continue
		}
		if e.malformed == "nan" {
			result = append(result, []float32{float32(math.NaN()), 0})
			continue
		}
		dimensions := e.dimensions
		if dimensions == 0 {
			dimensions = 2
		}
		vector := make([]float32, dimensions)
		switch {
		case strings.Contains(strings.ToLower(input), "charge") || strings.Contains(strings.ToLower(input), "payment"):
			vector[0] = 1
		case strings.Contains(strings.ToLower(input), "refund"):
			vector[0] = 0.8
			vector[1] = 0.2
		default:
			vector[len(vector)-1] = 1
		}
		result = append(result, vector)
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

	cache, err := embeddingcache.Open(ctx, filepath.Join(t.TempDir(), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	embedder := &fakeEmbedder{}
	service := semantic.NewService(repository, repository, cache, embedder).WithBatchSize(1)
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
	if third.Removed != 0 || third.Unchanged != 1 {
		t.Fatalf("deleted graph candidates affected cache-backed sync: %#v", third)
	}
	status, err := cache.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Rows != 2 {
		t.Fatalf("reusable cache row was deleted with graph candidate: %#v", status)
	}
}

func TestSyncDeduplicatesDocumentsAndForceReplacesUniqueKeys(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	duplicate := func(id string) graph.Node {
		return graph.Node{ID: id, Kind: graph.KindFunction, Name: "Charge", QualifiedName: "payments.Charge", OwnerFile: strings.TrimPrefix(id, "n:") + ".go"}
	}
	if err := repository.ReplaceOwner(ctx, "one.go", graph.ParseResult{Nodes: []graph.Node{duplicate("n:one")}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "two.go", graph.ParseResult{Nodes: []graph.Node{duplicate("n:two")}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cache, err := embeddingcache.Open(ctx, filepath.Join(t.TempDir(), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	embedder := &fakeEmbedder{}
	service := semantic.NewService(repository, repository, cache, embedder)
	report, err := service.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidates != 2 || report.Updated != 2 || embedder.inputs != 1 {
		t.Fatalf("deduplicated sync = %#v inputs=%d", report, embedder.inputs)
	}
	result, err := service.Search(ctx, "payment", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 2 || result.Matches[0].Node.ID != "n:one" || result.Matches[1].Node.ID != "n:two" {
		t.Fatalf("equal-document matches = %#v", result.Matches)
	}
	before := embedder.inputs
	report, err = service.WithForce(true).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated != 2 || embedder.inputs-before != 1 {
		t.Fatalf("forced sync = %#v new inputs=%d", report, embedder.inputs-before)
	}
}

func TestSyncRejectsProviderBatchFailuresWithoutPartialCommit(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed string
		err       error
	}{
		{name: "wrong count", malformed: "count"},
		{name: "mixed dimensions", malformed: "mixed"},
		{name: "non-finite", malformed: "nan"},
		{name: "cancellation", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = repository.Close() }()
			nodes := []graph.Node{
				{ID: "one", Kind: graph.KindFunction, Name: "Charge", QualifiedName: "payments.Charge", OwnerFile: "fixture.go"},
				{ID: "two", Kind: graph.KindFunction, Name: "Refund", QualifiedName: "payments.Refund", OwnerFile: "fixture.go"},
			}
			if err := repository.ReplaceOwner(ctx, "fixture.go", graph.ParseResult{Nodes: nodes}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			cache, err := embeddingcache.Open(ctx, filepath.Join(t.TempDir(), "embeddings.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cache.Close() }()
			embedder := &fakeEmbedder{malformed: test.malformed, err: test.err}
			_, err = semantic.NewService(repository, repository, cache, embedder).WithBatchSize(2).Sync(ctx)
			if err == nil || test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("Sync error = %v", err)
			}
			status, statusErr := cache.Status(ctx)
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			if status.Rows != 0 {
				t.Fatalf("failed provider batch committed %#v", status)
			}
		})
	}
}

func TestDimensionDriftRequiresForceForCurrentKeys(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	first := graph.Node{ID: "n:one", Kind: graph.KindFunction, Name: "Charge", QualifiedName: "payments.Charge", OwnerFile: "one.go"}
	if err := repository.ReplaceOwner(ctx, "one.go", graph.ParseResult{Nodes: []graph.Node{first}}); err != nil {
		t.Fatal(err)
	}
	cache, err := embeddingcache.Open(ctx, filepath.Join(t.TempDir(), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	embedder := &fakeEmbedder{dimensions: 2}
	service := semantic.NewService(repository, repository, cache, embedder)
	if _, err := service.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	second := graph.Node{ID: "n:two", Kind: graph.KindFunction, Name: "Refund", QualifiedName: "payments.Refund", OwnerFile: "two.go"}
	if err := repository.ReplaceOwner(ctx, "two.go", graph.ParseResult{Nodes: []graph.Node{second}}); err != nil {
		t.Fatal(err)
	}
	embedder.dimensions = 3
	if _, err := service.Sync(ctx); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("dimension drift error = %v", err)
	}
	if _, err := service.WithForce(true).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(ctx, "payment", 2); err != nil {
		t.Fatalf("search after forced replacement: %v", err)
	}
}

func TestForceSyncRecoversAfterPriorBatchLeavesMixedDimensions(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	nodes := []graph.Node{
		{ID: "n:charge", Kind: graph.KindFunction, Name: "Charge", QualifiedName: "payments.Charge", OwnerFile: "fixture.go"},
		{ID: "n:refund", Kind: graph.KindFunction, Name: "Refund", QualifiedName: "payments.Refund", OwnerFile: "fixture.go"},
	}
	if err := repository.ReplaceOwner(ctx, "fixture.go", graph.ParseResult{Nodes: nodes}); err != nil {
		t.Fatal(err)
	}
	cache, err := embeddingcache.Open(ctx, filepath.Join(t.TempDir(), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	embedder := &fakeEmbedder{dimensions: 2}
	service := semantic.NewService(repository, repository, cache, embedder).WithBatchSize(1)
	if _, err := service.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	embedder.dimensions = 3
	embedder.err = errors.New("second provider batch failed")
	embedder.failCall = embedder.calls + 2
	if _, err := service.WithForce(true).Sync(ctx); err == nil || !strings.Contains(err.Error(), "second provider batch failed") {
		t.Fatalf("partial force error = %v", err)
	}
	embedder.err = nil
	embedder.failCall = 0
	if _, err := service.Sync(ctx); err != nil {
		t.Fatalf("force did not recover mixed current dimensions: %v", err)
	}
	if _, err := service.Search(ctx, "payment", 2); err != nil {
		t.Fatalf("search after converged force: %v", err)
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
