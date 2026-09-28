package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

// Embedder is the provider boundary for semantic candidate discovery.
// Structural graph resolution never depends on this interface.
type Embedder interface {
	Model() string
	Embed(context.Context, []string) ([][]float32, error)
}

const DocumentVersion = "1"

// CacheKey is the source-free identity of one reusable embedding vector.
// Model and DocumentVersion are exact, case-sensitive namespaces.
type CacheKey struct {
	Model           string
	DocumentVersion string
	ContentHash     string
}

// CacheEntry is the validated vector stored for a content-addressed key.
type CacheEntry struct {
	Key    CacheKey
	Vector []float32
}

// EmbeddingCache is the persistence port for optional semantic vectors.
// Structural repositories deliberately do not implement this interface.
type EmbeddingCache interface {
	Load(context.Context, []CacheKey) (map[CacheKey][]float32, error)
	Store(context.Context, []CacheEntry) error
}

// CandidateRepository is the graph-backed port for semantic candidate
// selection. Embedding persistence belongs exclusively to EmbeddingCache.
type CandidateRepository interface {
	CandidateNodes(context.Context) ([]graph.Node, error)
}

type SyncReport struct {
	Model      string `json:"model"`
	Candidates int    `json:"candidates"`
	Updated    int    `json:"updated"`
	Unchanged  int    `json:"unchanged"`
	Removed    int64  `json:"removed"`
}

type Match struct {
	Node    graph.Node      `json:"node"`
	Score   float64         `json:"score"`
	Context query.Traversal `json:"context"`
}

type SearchResult struct {
	Query   string  `json:"query"`
	Model   string  `json:"model"`
	Matches []Match `json:"matches"`
}

type Service struct {
	candidates CandidateRepository
	graph      graph.QueryRepository
	cache      EmbeddingCache
	embedder   Embedder
	batchSize  int
	force      bool
	mu         sync.Mutex
}

func NewService(candidates CandidateRepository, graphRepository graph.QueryRepository, cache EmbeddingCache, embedder Embedder) *Service {
	return &Service{candidates: candidates, graph: graphRepository, cache: cache, embedder: embedder, batchSize: 32}
}

func (s *Service) WithBatchSize(size int) *Service {
	if size > 0 {
		s.batchSize = size
	}
	return s
}

func (s *Service) WithForce(force bool) *Service {
	s.force = force
	return s
}

func (s *Service) Sync(ctx context.Context) (SyncReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embedder == nil {
		return SyncReport{}, errors.New("semantic embedder is required")
	}
	if s.cache == nil {
		return SyncReport{}, errors.New("embedding cache is required")
	}
	model := s.embedder.Model()
	if strings.TrimSpace(model) == "" {
		return SyncReport{}, errors.New("embedding model is required")
	}
	nodes, err := s.candidates.CandidateNodes(ctx)
	if err != nil {
		return SyncReport{}, fmt.Errorf("list semantic candidates: %w", err)
	}
	type document struct {
		key   CacheKey
		text  string
		count int
	}
	byKey := make(map[CacheKey]*document, len(nodes))
	keys := make([]CacheKey, 0, len(nodes))
	for _, node := range nodes {
		text := Document(node)
		key := CacheKey{Model: model, DocumentVersion: DocumentVersion, ContentHash: contentHash(DocumentVersion + "\x00" + text)}
		if current, exists := byKey[key]; exists {
			current.count++
			continue
		}
		byKey[key] = &document{key: key, text: text, count: 1}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return cacheKeyLess(keys[i], keys[j]) })
	cached, err := s.cache.Load(ctx, keys)
	if err != nil {
		return SyncReport{}, fmt.Errorf("load embedding cache: %w", err)
	}
	report := SyncReport{Model: model, Candidates: len(nodes)}
	existingDimension := 0
	for _, key := range keys {
		vector, exists := cached[key]
		if !exists {
			continue
		}
		if existingDimension == 0 {
			existingDimension = len(vector)
		} else if len(vector) != existingDimension {
			return report, fmt.Errorf("embedding dimensions changed for model %q among current cache keys; run 'grafo embed --force'", model)
		}
		if !s.force {
			report.Unchanged += byKey[key].count
		}
	}
	pending := make([]*document, 0, len(keys))
	for _, key := range keys {
		if _, exists := cached[key]; !s.force && exists {
			continue
		}
		pending = append(pending, byKey[key])
	}
	providerDimension := 0
	for start := 0; start < len(pending); start += s.batchSize {
		end := min(start+s.batchSize, len(pending))
		texts := make([]string, 0, end-start)
		for _, document := range pending[start:end] {
			texts = append(texts, document.text)
		}
		vectors, err := s.embedder.Embed(ctx, texts)
		if err != nil {
			return report, fmt.Errorf("embed candidates: %w", err)
		}
		if len(vectors) != len(texts) {
			return report, fmt.Errorf("embed candidates: provider returned %d vectors for %d documents", len(vectors), len(texts))
		}
		entries := make([]CacheEntry, 0, len(vectors))
		for offset, vector := range vectors {
			vector, err = normalized(vector)
			if err != nil {
				return report, fmt.Errorf("embed cache key %s: %w", formatCacheKey(pending[start+offset].key), err)
			}
			if providerDimension == 0 {
				providerDimension = len(vector)
			} else if len(vector) != providerDimension {
				return report, fmt.Errorf("embed candidates: provider returned mixed dimensions %d and %d", providerDimension, len(vector))
			}
			if !s.force && existingDimension != 0 && len(vector) != existingDimension {
				return report, fmt.Errorf("embedding dimensions changed for model %q from %d to %d; run 'grafo embed --force'", model, existingDimension, len(vector))
			}
			entries = append(entries, CacheEntry{Key: pending[start+offset].key, Vector: vector})
		}
		if err := s.cache.Store(ctx, entries); err != nil {
			return report, fmt.Errorf("store embedding cache batch: %w", err)
		}
		for _, document := range pending[start:end] {
			report.Updated += document.count
		}
	}
	return report, nil
}

func (s *Service) Search(ctx context.Context, text string, limit int) (SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embedder == nil {
		return SearchResult{}, errors.New("semantic embedder is required")
	}
	if s.cache == nil {
		return SearchResult{}, errors.New("embedding cache is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return SearchResult{}, errors.New("semantic query is required")
	}
	if limit <= 0 {
		limit = 5
	}
	model := s.embedder.Model()
	if strings.TrimSpace(model) == "" {
		return SearchResult{}, errors.New("embedding model is required")
	}
	nodes, err := s.candidates.CandidateNodes(ctx)
	if err != nil {
		return SearchResult{}, fmt.Errorf("list semantic candidates: %w", err)
	}
	keys := make([]CacheKey, 0, len(nodes))
	nodeKeys := make([]CacheKey, 0, len(nodes))
	seen := make(map[CacheKey]struct{}, len(nodes))
	for _, node := range nodes {
		key := CacheKey{Model: model, DocumentVersion: DocumentVersion, ContentHash: contentHash(DocumentVersion + "\x00" + Document(node))}
		nodeKeys = append(nodeKeys, key)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return cacheKeyLess(keys[i], keys[j]) })
	embeddings, err := s.cache.Load(ctx, keys)
	if err != nil {
		return SearchResult{}, fmt.Errorf("load embedding cache: %w", err)
	}
	for _, key := range keys {
		if _, exists := embeddings[key]; !exists {
			return SearchResult{}, fmt.Errorf("no embedding for current cache key %s; run semantic sync first", formatCacheKey(key))
		}
	}
	vectors, err := s.embedder.Embed(ctx, []string{text})
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
	if len(nodes) == 0 {
		return SearchResult{}, fmt.Errorf("no embeddings for model %q; run semantic sync first", model)
	}
	type scoredNode struct {
		node  graph.Node
		score float64
	}
	scored := make([]scoredNode, 0, len(nodes))
	for index, node := range nodes {
		vector := embeddings[nodeKeys[index]]
		if len(vector) != len(queryVector) {
			return SearchResult{}, fmt.Errorf("embedding dimensions changed for model %q: stored %d, query %d", model, len(vector), len(queryVector))
		}
		scored = append(scored, scoredNode{node: node, score: dot(queryVector, vector)})
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
	if len(scored) > limit {
		scored = scored[:limit]
	}
	result := SearchResult{Query: text, Model: model, Matches: make([]Match, 0, len(scored))}
	graphQuery := query.NewService(s.graph)
	for _, candidate := range scored {
		contextGraph, err := graphQuery.Neighborhood(ctx, candidate.node.ID, "", 1, query.Both, nil, 50)
		if err != nil {
			return SearchResult{}, fmt.Errorf("resolve context for %s: %w", candidate.node.QualifiedName, err)
		}
		result.Matches = append(result.Matches, Match{Node: candidate.node, Score: candidate.score, Context: contextGraph})
	}
	return result, nil
}

// Document creates stable, source-free semantic input from indexed metadata.
func Document(node graph.Node) string {
	parts := []string{string(node.Kind), splitIdentifier(node.Name), splitIdentifier(node.QualifiedName)}
	if node.Language != "" {
		parts = append(parts, "language "+node.Language)
	}
	for _, key := range graph.SortedPropertyKeys(node.Properties) {
		parts = append(parts, splitIdentifier(key)+" "+splitIdentifier(node.Properties[key]))
	}
	return strings.Join(parts, "\n")
}

func splitIdentifier(value string) string {
	var result []rune
	var previous rune
	for index, current := range []rune(value) {
		if index > 0 && unicode.IsUpper(current) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			result = append(result, ' ')
		}
		switch current {
		case '.', '/', ':', '_', '-', '(', ')', '[', ']', '{', '}', ',':
			result = append(result, ' ')
		default:
			result = append(result, current)
		}
		previous = current
	}
	return strings.Join(strings.Fields(string(result)), " ")
}

func contentHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func cacheKeyLess(left, right CacheKey) bool {
	if left.Model != right.Model {
		return left.Model < right.Model
	}
	if left.DocumentVersion != right.DocumentVersion {
		return left.DocumentVersion < right.DocumentVersion
	}
	return left.ContentHash < right.ContentHash
}

func formatCacheKey(key CacheKey) string {
	return fmt.Sprintf("(%q,%q,%s)", key.Model, key.DocumentVersion, key.ContentHash)
}

func normalized(vector []float32) ([]float32, error) {
	if len(vector) == 0 {
		return nil, errors.New("provider returned an empty vector")
	}
	var magnitude float64
	result := make([]float32, len(vector))
	for index, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("provider returned a non-finite vector")
		}
		magnitude += float64(value) * float64(value)
		result[index] = value
	}
	if magnitude == 0 {
		return nil, errors.New("provider returned a zero vector")
	}
	scale := float32(1 / math.Sqrt(magnitude))
	for index := range result {
		result[index] *= scale
	}
	return result, nil
}

func dot(left, right []float32) float64 {
	var result float64
	for index := range left {
		result += float64(left[index]) * float64(right[index])
	}
	return result
}
