package semantic_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddingcache "github.com/cafecito-games/grafo/internal/embedding/cache"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// budgetEmbedder makes provider latency explicit so a time budget can be
// exhausted deterministically instead of by wall-clock luck.
type budgetEmbedder struct {
	delay     time.Duration
	documents []string
	calls     int
}

func (*budgetEmbedder) Model() string { return "budget-model" }

func (e *budgetEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	e.documents = append(e.documents, inputs...)
	time.Sleep(e.delay)
	result := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		if strings.Contains(strings.ToLower(input), "charge") {
			result = append(result, []float32{1, 0})
			continue
		}
		result = append(result, []float32{0, 1})
	}
	return result, nil
}

func cacheKeyFor(model string, node graph.Node) semantic.CacheKey {
	digest := sha256.Sum256([]byte(semantic.DocumentVersion + "\x00" + semantic.Document(node)))
	return semantic.CacheKey{Model: model, DocumentVersion: semantic.DocumentVersion,
		ContentHash: hex.EncodeToString(digest[:])}
}

// searchFixture indexes candidate nodes and returns them exactly as candidate
// selection sees them, so a test can pre-warm the cache for a chosen subset.
func searchFixture(t *testing.T, ctx context.Context, count int) (*sqlite.Repository, []graph.Node) {
	t.Helper()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	nodes := make([]graph.Node, 0, count)
	for index := range count {
		name := fmt.Sprintf("Charge%02d", index)
		language, path := "go", fmt.Sprintf("internal/payments/charge%02d.go", index)
		if index%2 == 1 {
			name = fmt.Sprintf("Mail%02d", index)
			language, path = "gdscript", fmt.Sprintf("scenes/mail%02d.gd", index)
		}
		nodes = append(nodes, graph.Node{ID: fmt.Sprintf("n:%02d", index), Kind: graph.KindFunction,
			Name: name, QualifiedName: "fixture." + name, Language: language,
			Location: graph.Location{Path: path, Line: 1}, OwnerFile: path})
	}
	for _, node := range nodes {
		if err := repository.ReplaceOwner(ctx, node.OwnerFile, graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	candidates, err := repository.CandidateNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != count {
		t.Fatalf("candidate nodes = %d, want %d", len(candidates), count)
	}
	return repository, candidates
}

func openCache(t *testing.T, ctx context.Context) *embeddingcache.Cache {
	t.Helper()
	cache, err := embeddingcache.Open(ctx, filepath.Join(testtemp.Dir(t), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

// warm stores vectors for the first count candidates, mirroring what a
// completed backfill would have left in the cache.
func warm(t *testing.T, ctx context.Context, cache *embeddingcache.Cache, nodes []graph.Node, count int) {
	t.Helper()
	entries := make([]semantic.CacheEntry, 0, count)
	for _, node := range nodes[:count] {
		vector := []float32{0, 1}
		if strings.Contains(strings.ToLower(semantic.Document(node)), "charge") {
			vector = []float32{1, 0}
		}
		entries = append(entries, semantic.CacheEntry{Key: cacheKeyFor("budget-model", node), Vector: vector})
	}
	if err := cache.Store(ctx, entries); err != nil {
		t.Fatal(err)
	}
}

// TestSearchWithBoundsEveryPhaseInsideTheBudget pins the three answers a
// bounded reusable-code query may give: a complete ranking on a warm cache, a
// marked partial answer when the budget runs out, and an actionable warming
// answer when embedding coverage is still too low to rank honestly.
func TestSearchWithBoundsEveryPhaseInsideTheBudget(t *testing.T) {
	const candidates = 20
	for _, test := range []struct {
		name string
		// warmed is how many candidate documents are already cached.
		warmed  int
		delay   time.Duration
		budget  time.Duration
		limit   int
		request func(semantic.SearchRequest) semantic.SearchRequest

		wantStatus      semantic.SearchStatus
		wantPartial     bool
		wantMatches     int
		wantEmbedded    int
		wantBackfilled  int
		wantNoteFragmnt string
		wantAction      string
		// wantCandidateDocuments is how many candidate documents the provider
		// may be asked to embed; the query document is never counted.
		wantCandidateDocuments int
	}{
		{
			name:   "warm cache returns the complete ranking",
			warmed: candidates, delay: 0, budget: 30 * time.Second, limit: 3,
			wantStatus: semantic.StatusComplete, wantPartial: false, wantMatches: 3,
			wantEmbedded: candidates, wantCandidateDocuments: 0,
		},
		{
			name:   "cold cache reports warming with progress",
			warmed: 0, delay: 20 * time.Millisecond, budget: time.Millisecond, limit: 3,
			wantStatus: semantic.StatusWarming, wantPartial: true, wantMatches: 0,
			wantEmbedded: 0, wantCandidateDocuments: 0,
			wantNoteFragmnt: "candidate documents are not embedded yet",
			wantAction:      "embedding cache is warming",
		},
		{
			name:   "exhausted budget leaves embedding coverage incomplete",
			warmed: candidates - 1, delay: 20 * time.Millisecond, budget: time.Millisecond, limit: 1,
			wantStatus: semantic.StatusPartial, wantPartial: true, wantMatches: 1,
			wantEmbedded: candidates - 1, wantCandidateDocuments: 0,
			wantNoteFragmnt: "1 of 20 candidate documents are not embedded yet",
			wantAction:      "results are partial within the time budget",
		},
		{
			name:   "exhausted budget stops graph resolution after the first match",
			warmed: candidates, delay: 20 * time.Millisecond, budget: time.Millisecond, limit: 3,
			wantStatus: semantic.StatusPartial, wantPartial: true, wantMatches: 1,
			wantEmbedded: candidates, wantCandidateDocuments: 0,
			wantNoteFragmnt: "graph context resolution stopped after 1 of 3 ranked matches",
			wantAction:      "results are partial within the time budget",
		},
		{
			name:   "filters bound the candidate set and the backfill it implies",
			warmed: 0, delay: 0, budget: 30 * time.Second, limit: 3,
			request: func(request semantic.SearchRequest) semantic.SearchRequest {
				request.Languages = []string{"GDScript"}
				request.PathPrefixes = []string{"scenes"}
				return request
			},
			wantStatus: semantic.StatusComplete, wantPartial: false, wantMatches: 3,
			wantEmbedded: candidates / 2, wantBackfilled: candidates / 2,
			wantCandidateDocuments: candidates / 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, nodes := searchFixture(t, ctx, candidates)
			cache := openCache(t, ctx)
			warm(t, ctx, cache, nodes, test.warmed)
			embedder := &budgetEmbedder{delay: test.delay}
			service := semantic.NewService(repository, repository, cache, embedder).WithBatchSize(4)

			request := semantic.SearchRequest{Query: "charge a card", Limit: test.limit, Budget: test.budget}
			if test.request != nil {
				request = test.request(request)
			}
			result, err := service.SearchWith(ctx, request)
			if err != nil {
				t.Fatalf("SearchWith: %v", err)
			}
			if result.Status != test.wantStatus || result.Partial != test.wantPartial {
				t.Fatalf("status = %q partial = %t, want %q/%t (notes %v)",
					result.Status, result.Partial, test.wantStatus, test.wantPartial, result.Notes)
			}
			if len(result.Matches) != test.wantMatches {
				t.Fatalf("matches = %d, want %d", len(result.Matches), test.wantMatches)
			}
			if result.Coverage.Embedded != test.wantEmbedded || result.Coverage.Backfilled != test.wantBackfilled {
				t.Fatalf("coverage = %#v, want embedded %d backfilled %d",
					result.Coverage, test.wantEmbedded, test.wantBackfilled)
			}
			if result.Coverage.Missing != result.Coverage.Documents-result.Coverage.Embedded {
				t.Fatalf("coverage missing is inconsistent: %#v", result.Coverage)
			}
			if got := len(embedder.documents) - 1; got != test.wantCandidateDocuments {
				t.Fatalf("provider embedded %d candidate documents, want %d", got, test.wantCandidateDocuments)
			}
			if test.wantNoteFragmnt != "" && !containsFragment(result.Notes, test.wantNoteFragmnt) {
				t.Fatalf("notes = %#v, want one containing %q", result.Notes, test.wantNoteFragmnt)
			}
			if test.wantAction != "" && !strings.Contains(result.NextAction, test.wantAction) {
				t.Fatalf("next action = %q, want it to contain %q", result.NextAction, test.wantAction)
			}
			if test.wantStatus == semantic.StatusComplete {
				if len(result.Notes) != 0 || result.NextAction != "" {
					t.Fatalf("a complete answer carried partial notes: %#v / %q", result.Notes, result.NextAction)
				}
			}
			assertSpanOrder(t, result.Timings)
			for index := 1; index < len(result.Matches); index++ {
				if result.Matches[index-1].Score < result.Matches[index].Score {
					t.Fatalf("matches are not ordered by descending score: %#v", result.Matches)
				}
			}
		})
	}
}

// TestSearchOnWarmCacheReturnsTheSameRankingAsTheUnboundedQuery proves the
// bounded path did not change what a fully embedded index answers: the same
// candidates, in the same order, with the same one-hop context.
func TestSearchOnWarmCacheReturnsTheSameRankingAsTheUnboundedQuery(t *testing.T) {
	ctx := context.Background()
	repository, nodes := searchFixture(t, ctx, 6)
	cache := openCache(t, ctx)
	embedder := &budgetEmbedder{}
	service := semantic.NewService(repository, repository, cache, embedder).WithBatchSize(2)
	if _, err := service.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := service.SearchWith(ctx, semantic.SearchRequest{Query: "charge a card", Limit: 3,
		Budget: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != semantic.StatusComplete || result.Partial {
		t.Fatalf("warm cache status = %q partial = %t", result.Status, result.Partial)
	}
	if result.Coverage.Fraction != 1 || result.Coverage.Candidates != len(nodes) {
		t.Fatalf("warm coverage = %#v", result.Coverage)
	}
	want := []string{"fixture.Charge00", "fixture.Charge02", "fixture.Charge04"}
	got := make([]string, 0, len(result.Matches))
	for _, match := range result.Matches {
		got = append(got, match.Node.QualifiedName)
		if len(match.Context.Nodes) == 0 {
			t.Fatalf("match %s lacks structural context", match.Node.QualifiedName)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ranking = %v, want %v", got, want)
	}

	// The legacy entry point must keep answering identically.
	legacy, err := service.Search(ctx, "charge a card", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Matches) != len(result.Matches) {
		t.Fatalf("Search and SearchWith disagree: %d vs %d matches", len(legacy.Matches), len(result.Matches))
	}
	for index := range legacy.Matches {
		if legacy.Matches[index].Node.ID != result.Matches[index].Node.ID ||
			legacy.Matches[index].Score != result.Matches[index].Score {
			t.Fatalf("legacy match %d = %#v, want %#v", index, legacy.Matches[index], result.Matches[index])
		}
	}
}

func TestSearchWithRejectsUnusableRequests(t *testing.T) {
	ctx := context.Background()
	repository, _ := searchFixture(t, ctx, 2)
	cache := openCache(t, ctx)
	service := semantic.NewService(repository, repository, cache, &budgetEmbedder{})
	for _, test := range []struct {
		name    string
		request semantic.SearchRequest
		want    string
	}{
		{name: "blank query", request: semantic.SearchRequest{Query: "   "}, want: "semantic query is required"},
		{name: "escaping path prefix", request: semantic.SearchRequest{Query: "charge", PathPrefixes: []string{"../etc"}},
			want: "invalid path prefix"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.SearchWith(ctx, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("SearchWith error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestSearchWithReportsNoCandidateForUnmatchedFilters(t *testing.T) {
	ctx := context.Background()
	repository, nodes := searchFixture(t, ctx, 4)
	cache := openCache(t, ctx)
	embedder := &budgetEmbedder{}
	service := semantic.NewService(repository, repository, cache, embedder)
	warm(t, ctx, cache, nodes, len(nodes))

	result, err := service.SearchWith(ctx, semantic.SearchRequest{Query: "charge a card",
		Languages: []string{"rust"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != semantic.StatusComplete || len(result.Matches) != 0 || result.Coverage.Candidates != 0 {
		t.Fatalf("filtered-out query = %#v", result)
	}
	if !containsFragment(result.Notes, "no candidate matched the requested filters") {
		t.Fatalf("notes = %#v", result.Notes)
	}
	if embedder.calls != 0 {
		t.Fatalf("an unmatched filter still contacted the provider %d times", embedder.calls)
	}
}

func containsFragment(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

// assertSpanOrder keeps the instrumentation contract: every phase is reported
// once, in a fixed order, so a caller can compare runs.
func assertSpanOrder(t *testing.T, timings semantic.Timings) {
	t.Helper()
	want := []string{"candidate_selection", "query_embedding", "embedding_cache_load",
		"embedding_backfill", "ranking", "graph_resolution", "response_assembly"}
	got := make([]string, 0, len(timings.Spans))
	for _, span := range timings.Spans {
		got = append(got, span.Name)
		if span.Milliseconds < 0 {
			t.Fatalf("span %s has a negative duration", span.Name)
		}
	}
	if len(got) == 1 && got[0] == "candidate_selection" {
		return
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spans = %v, want %v", got, want)
	}
}
