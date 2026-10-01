package semantic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/pathscope"
	"github.com/cafecito-games/grafo/internal/query"
)

// SearchStatus names how completely a reusable-code query was answered.
// Callers must treat anything other than StatusComplete as a bounded,
// explicitly partial view of the index.
type SearchStatus string

const (
	// StatusComplete means every current candidate document was embedded and
	// ranked, and graph context resolved for every returned match.
	StatusComplete SearchStatus = "complete"
	// StatusPartial means ranking or graph resolution stopped early, so the
	// returned matches are the best bounded answer rather than the full one.
	StatusPartial SearchStatus = "partial"
	// StatusWarming means embedding coverage is still too low to rank the
	// candidate set honestly. Any matches are a preview of a warming cache.
	StatusWarming SearchStatus = "warming"
)

const (
	// DefaultSearchBudget bounds one reusable-code query. It stays well below
	// the 300s timeout common MCP clients apply, so an exhausted budget
	// produces a marked partial response instead of a client-side timeout.
	DefaultSearchBudget = 45 * time.Second
	// MaxSearchBudget caps a caller-supplied budget.
	MaxSearchBudget = 240 * time.Second
	// DefaultMinimumCoverage is the embedded fraction of current candidate
	// documents at or above which ranking is reported as trustworthy.
	DefaultMinimumCoverage = 0.9
	// backfillBudgetShare reserves part of the budget for query embedding,
	// ranking, and graph resolution so backfill can never consume all of it.
	backfillBudgetShare = 0.6
)

// SearchRequest is the bounded form of a reusable-code query. Every field is
// optional except Query, so an empty request behaves like the historical
// Search(ctx, text, limit) call with default bounds applied.
type SearchRequest struct {
	Query string
	Limit int
	// Budget bounds the whole query. A non-positive value selects
	// DefaultSearchBudget; values above MaxSearchBudget are clamped.
	Budget time.Duration
	// MinimumCoverage is the embedded fraction required before ranked results
	// are reported as anything but StatusWarming. Non-positive selects
	// DefaultMinimumCoverage.
	MinimumCoverage float64
	// Languages restricts candidates to these node languages, compared
	// case-insensitively.
	Languages []string
	// PathPrefixes restricts candidates to nodes whose location path lies
	// under one of these repository-relative segment prefixes.
	PathPrefixes []string
}

// Coverage reports how much of the current candidate set could be ranked. It
// separates "waiting on embedding-cache backfill" from ranking work.
type Coverage struct {
	// Candidates is the number of graph candidates after filters.
	Candidates int `json:"candidates"`
	// Documents is the number of distinct embedding documents those
	// candidates reduce to.
	Documents int `json:"documents"`
	// Embedded is how many documents had a vector available for ranking.
	Embedded int `json:"embedded"`
	// Missing is how many documents still lack a vector.
	Missing int `json:"missing"`
	// Backfilled is how many documents this call embedded and cached.
	Backfilled int `json:"backfilled"`
	// RankedCandidates is how many candidates were scored.
	RankedCandidates int `json:"ranked_candidates"`
	// ResolvedMatches is how many returned matches carry graph context.
	ResolvedMatches int `json:"resolved_matches"`
	// Fraction is Embedded/Documents, rounded to four decimals.
	Fraction float64 `json:"fraction"`
}

// Span is one measured phase of a reusable-code query.
type Span struct {
	Name         string `json:"name"`
	Milliseconds int64  `json:"milliseconds"`
}

// Timings reports phase durations in a fixed, deterministic span order.
type Timings struct {
	Spans             []Span `json:"spans"`
	TotalMilliseconds int64  `json:"total_milliseconds"`
}

// BudgetReport states the bound that was applied and whether it ran out.
type BudgetReport struct {
	Milliseconds int64 `json:"milliseconds"`
	Exhausted    bool  `json:"exhausted"`
}

// SearchFilters echoes the normalized candidate filters back to the caller.
type SearchFilters struct {
	Languages    []string `json:"languages,omitempty"`
	PathPrefixes []string `json:"path_prefixes,omitempty"`
}

type spanRecorder struct {
	started time.Time
	spans   []Span
}

func newSpanRecorder(started time.Time) *spanRecorder {
	return &spanRecorder{started: started, spans: []Span{}}
}

// record appends one span. Phases are recorded in call order, which is fixed
// by Search, so the span sequence is deterministic even though durations are
// not.
func (r *spanRecorder) record(name string, since time.Time, now time.Time) {
	r.spans = append(r.spans, Span{Name: name, Milliseconds: milliseconds(now.Sub(since))})
}

func (r *spanRecorder) timings(now time.Time) Timings {
	return Timings{Spans: r.spans, TotalMilliseconds: milliseconds(now.Sub(r.started))}
}

func milliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

// Search ranks candidates for a natural-language description with default
// bounds. It is the backward-compatible form of SearchWith.
func (s *Service) Search(ctx context.Context, text string, limit int) (SearchResult, error) {
	return s.SearchWith(ctx, SearchRequest{Query: text, Limit: limit})
}

// SearchWith answers a reusable-code query inside a time budget.
//
// The query never blocks on a full embedding backfill. Missing documents are
// embedded in deterministic cache-key order until the backfill share of the
// budget runs out; ranking then covers exactly the documents that have
// vectors, and the response states coverage, the applied budget, and per-phase
// timings. Ordering of ranked matches is unchanged from the unbounded path, so
// a warm cache returns the same matches it always did.
func (s *Service) SearchWith(ctx context.Context, request SearchRequest) (SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	spans := newSpanRecorder(start)

	request, err := request.normalized()
	if err != nil {
		return SearchResult{}, err
	}
	if s.embedder == nil {
		return SearchResult{}, errors.New("semantic embedder is required")
	}
	if s.cache == nil {
		return SearchResult{}, errors.New("embedding cache is required")
	}
	model := s.embedder.Model()
	if strings.TrimSpace(model) == "" {
		return SearchResult{}, errors.New("embedding model is required")
	}
	deadline := start.Add(request.Budget)
	backfillDeadline := start.Add(time.Duration(float64(request.Budget) * backfillBudgetShare))

	phase := time.Now()
	nodes, err := s.candidates.CandidateNodes(ctx)
	if err != nil {
		return SearchResult{}, fmt.Errorf("list semantic candidates: %w", err)
	}
	total := len(nodes)
	nodes = request.filter(nodes)
	selection := newSelection(model, nodes)
	spans.record("candidate_selection", phase, time.Now())

	result := SearchResult{Query: request.Query, Model: model, Matches: []Match{},
		Filters: SearchFilters{Languages: request.Languages, PathPrefixes: request.PathPrefixes},
		Budget:  BudgetReport{Milliseconds: request.Budget.Milliseconds()}}

	if total == 0 {
		return SearchResult{}, fmt.Errorf("no embeddings for model %q; run semantic sync first", model)
	}
	if len(nodes) == 0 {
		result.Status = StatusComplete
		result.Notes = []string{fmt.Sprintf("no candidate matched the requested filters; %d candidates were considered", total)}
		result.NextAction = "widen or drop the language and path filters"
		result.Timings = spans.timings(time.Now())
		return result, nil
	}

	phase = time.Now()
	vectors, err := s.embedder.Embed(ctx, []string{request.Query})
	if err != nil {
		return SearchResult{}, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return SearchResult{}, fmt.Errorf("embed query: provider returned %d vectors", len(vectors))
	}
	queryVector, err := normalized(vectors[0])
	if err != nil {
		return SearchResult{}, fmt.Errorf("embed query: %w", err)
	}
	spans.record("query_embedding", phase, time.Now())

	phase = time.Now()
	embeddings, err := s.cache.Load(ctx, selection.keys)
	if err != nil {
		return SearchResult{}, fmt.Errorf("load embedding cache: %w", err)
	}
	spans.record("embedding_cache_load", phase, time.Now())

	phase = time.Now()
	backfilled, backfillErr := s.backfill(ctx, selection, embeddings, backfillDeadline)
	spans.record("embedding_backfill", phase, time.Now())

	embedded := 0
	for _, key := range selection.keys {
		if _, exists := embeddings[key]; exists {
			embedded++
		}
	}
	result.Coverage = Coverage{Candidates: len(nodes), Documents: len(selection.keys),
		Embedded: embedded, Missing: len(selection.keys) - embedded, Backfilled: backfilled,
		Fraction: fraction(embedded, len(selection.keys))}
	if backfillErr != nil {
		if embedded == 0 {
			return SearchResult{}, backfillErr
		}
		result.Notes = append(result.Notes, "embedding backfill stopped early: "+backfillErr.Error())
	}
	if result.Coverage.Missing > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d of %d candidate documents are not embedded yet; this call embedded %d within its backfill budget",
			result.Coverage.Missing, result.Coverage.Documents, backfilled))
	}

	phase = time.Now()
	scored := selection.rank(embeddings, queryVector, model)
	if scored.err != nil {
		return SearchResult{}, scored.err
	}
	result.Coverage.RankedCandidates = len(scored.nodes)
	ranked := scored.nodes
	if len(ranked) > request.Limit {
		ranked = ranked[:request.Limit]
	}
	spans.record("ranking", phase, time.Now())

	phase = time.Now()
	graphQuery := query.NewService(s.graph)
	resolutionStopped := false
	for index, candidate := range ranked {
		if index > 0 && !time.Now().Before(deadline) {
			resolutionStopped = true
			result.Notes = append(result.Notes, fmt.Sprintf(
				"graph context resolution stopped after %d of %d ranked matches when the time budget ran out",
				index, len(ranked)))
			break
		}
		contextGraph, err := graphQuery.Neighborhood(ctx, candidate.node.ID, "", 1, query.Both, nil, 50)
		if err != nil {
			return SearchResult{}, fmt.Errorf("resolve context for %s: %w", candidate.node.QualifiedName, err)
		}
		result.Matches = append(result.Matches, Match{Node: candidate.node, Score: candidate.score, Context: contextGraph})
	}
	result.Coverage.ResolvedMatches = len(result.Matches)
	spans.record("graph_resolution", phase, time.Now())

	phase = time.Now()
	result.applyStatus(request, resolutionStopped, backfillDeadline, deadline)
	assembled := time.Now()
	spans.record("response_assembly", phase, assembled)
	result.Timings = spans.timings(assembled)
	return result, nil
}

// applyStatus decides the single honest status for a bounded query. Warming
// outranks partial because insufficient coverage makes the ranking itself
// unrepresentative, not merely truncated.
func (r *SearchResult) applyStatus(request SearchRequest, resolutionStopped bool, backfillDeadline, deadline time.Time) {
	now := time.Now()
	budgetExhausted := resolutionStopped || !now.Before(deadline) ||
		(r.Coverage.Missing > 0 && !now.Before(backfillDeadline))
	r.Budget.Exhausted = budgetExhausted
	switch {
	case r.Coverage.Documents > 0 && r.Coverage.Fraction < request.MinimumCoverage:
		r.Status = StatusWarming
		r.Partial = true
		r.NextAction = fmt.Sprintf(
			"embedding cache is warming: %d of %d documents embedded (%.1f%%); run 'grafo embed' to finish warming, or narrow the query with language/path filters",
			r.Coverage.Embedded, r.Coverage.Documents, r.Coverage.Fraction*100)
	case r.Coverage.Missing > 0 || resolutionStopped:
		r.Status = StatusPartial
		r.Partial = true
		r.NextAction = "results are partial within the time budget; run 'grafo embed' to complete coverage or raise the budget"
	default:
		r.Status = StatusComplete
		r.Partial = false
	}
}

func fraction(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(int(float64(part)/float64(whole)*10000+0.5)) / 10000
}

func (r SearchRequest) normalized() (SearchRequest, error) {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" {
		return r, errors.New("semantic query is required")
	}
	if r.Limit <= 0 {
		r.Limit = 5
	}
	if r.Budget <= 0 {
		r.Budget = DefaultSearchBudget
	}
	if r.Budget > MaxSearchBudget {
		r.Budget = MaxSearchBudget
	}
	if r.MinimumCoverage <= 0 {
		r.MinimumCoverage = DefaultMinimumCoverage
	}
	if r.MinimumCoverage > 1 {
		r.MinimumCoverage = 1
	}
	r.Languages = normalizedLanguages(r.Languages)
	prefixes, err := normalizedPathPrefixes(r.PathPrefixes)
	if err != nil {
		return r, err
	}
	r.PathPrefixes = prefixes
	return r, nil
}

func normalizedLanguages(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		set[value] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizedPathPrefixes(values []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		prefix, err := pathscope.NormalizePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid path prefix %q: %w", value, err)
		}
		set[prefix] = struct{}{}
	}
	if len(set) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

// filter keeps candidate order, so filtering never changes relative ranking.
func (r SearchRequest) filter(nodes []graph.Node) []graph.Node {
	if len(r.Languages) == 0 && len(r.PathPrefixes) == 0 {
		return nodes
	}
	result := make([]graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if !r.matches(node) {
			continue
		}
		result = append(result, node)
	}
	return result
}

func (r SearchRequest) matches(node graph.Node) bool {
	if len(r.Languages) > 0 {
		language := strings.ToLower(node.Language)
		found := false
		for _, candidate := range r.Languages {
			if candidate == language {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.PathPrefixes) > 0 {
		path := node.Location.Path
		if path == "" {
			return false
		}
		for _, prefix := range r.PathPrefixes {
			if pathscope.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	return true
}

// selection is the deduplicated candidate set of one query: node order for
// ranking, and sorted unique cache keys for cache and backfill work.
type selection struct {
	nodes    []graph.Node
	nodeKeys []CacheKey
	keys     []CacheKey
	texts    map[CacheKey]string
}

func newSelection(model string, nodes []graph.Node) selection {
	result := selection{nodes: nodes, nodeKeys: make([]CacheKey, 0, len(nodes)),
		keys: make([]CacheKey, 0, len(nodes)), texts: make(map[CacheKey]string, len(nodes))}
	for _, node := range nodes {
		text := Document(node)
		key := CacheKey{Model: model, DocumentVersion: DocumentVersion, ContentHash: contentHash(DocumentVersion + "\x00" + text)}
		result.nodeKeys = append(result.nodeKeys, key)
		if _, exists := result.texts[key]; exists {
			continue
		}
		result.texts[key] = text
		result.keys = append(result.keys, key)
	}
	sort.Slice(result.keys, func(i, j int) bool { return cacheKeyLess(result.keys[i], result.keys[j]) })
	return result
}

type rankedCandidate struct {
	node  graph.Node
	score float64
}

type ranking struct {
	nodes []rankedCandidate
	err   error
}

// rank scores exactly the candidates whose document has a vector. Candidates
// without one are left out rather than scored against a guessed vector.
func (s selection) rank(embeddings map[CacheKey][]float32, queryVector []float32, model string) ranking {
	scored := make([]rankedCandidate, 0, len(s.nodes))
	for index, node := range s.nodes {
		vector, exists := embeddings[s.nodeKeys[index]]
		if !exists {
			continue
		}
		if len(vector) != len(queryVector) {
			return ranking{err: fmt.Errorf("embedding dimensions changed for model %q: stored %d, query %d",
				model, len(vector), len(queryVector))}
		}
		scored = append(scored, rankedCandidate{node: node, score: dot(queryVector, vector)})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].node.QualifiedName != scored[j].node.QualifiedName {
			return scored[i].node.QualifiedName < scored[j].node.QualifiedName
		}
		return scored[i].node.ID < scored[j].node.ID
	})
	return ranking{nodes: scored}
}

// backfill embeds missing documents in deterministic cache-key order until the
// backfill deadline passes. It reports how many documents it embedded and, if
// it stopped on a provider or cache failure, why. Reaching the deadline is not
// an error: the caller degrades to a marked partial or warming response.
func (s *Service) backfill(ctx context.Context, candidates selection, embeddings map[CacheKey][]float32, deadline time.Time) (int, error) {
	pending := make([]CacheKey, 0, len(candidates.keys))
	for _, key := range candidates.keys {
		if _, exists := embeddings[key]; !exists {
			pending = append(pending, key)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	dimension := 0
	for _, vector := range embeddings {
		dimension = len(vector)
		break
	}
	embedded := 0
	for start := 0; start < len(pending); start += s.batchSize {
		if !time.Now().Before(deadline) {
			return embedded, nil
		}
		if err := ctx.Err(); err != nil {
			return embedded, err
		}
		end := min(start+s.batchSize, len(pending))
		batch := pending[start:end]
		texts := make([]string, 0, len(batch))
		for _, key := range batch {
			texts = append(texts, candidates.texts[key])
		}
		vectors, err := s.embedder.Embed(ctx, texts)
		if err != nil {
			return embedded, fmt.Errorf("embed candidates: %w", err)
		}
		if len(vectors) != len(texts) {
			return embedded, fmt.Errorf("embed candidates: provider returned %d vectors for %d documents", len(vectors), len(texts))
		}
		entries := make([]CacheEntry, 0, len(vectors))
		accepted := make([][]float32, 0, len(vectors))
		for offset, vector := range vectors {
			vector, err = normalized(vector)
			if err != nil {
				return embedded, fmt.Errorf("embed cache key %s: %w", formatCacheKey(batch[offset]), err)
			}
			if dimension == 0 {
				dimension = len(vector)
			} else if len(vector) != dimension {
				return embedded, fmt.Errorf("embedding dimensions changed for model %q from %d to %d; run 'grafo embed --force'",
					s.embedder.Model(), dimension, len(vector))
			}
			entries = append(entries, CacheEntry{Key: batch[offset], Vector: vector})
			accepted = append(accepted, vector)
		}
		if err := s.cache.Store(ctx, entries); err != nil {
			return embedded, fmt.Errorf("store embedding cache batch: %w", err)
		}
		for offset, entry := range entries {
			embeddings[entry.Key] = accepted[offset]
		}
		embedded += len(entries)
	}
	return embedded, nil
}
