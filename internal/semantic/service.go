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

type Embedding struct {
	NodeID      string
	Model       string
	ContentHash string
	Vector      []float32
	UpdatedAt   string
}

// ReadRepository is the persistence port required by semantic candidate
// discovery and matching. It deliberately excludes embedding writes so a
// query-only storage handle can expose semantic search safely.
type ReadRepository interface {
	CandidateNodes(context.Context) ([]graph.Node, error)
	EmbeddingHashes(context.Context, string) (map[string]string, error)
	Embeddings(context.Context, string) ([]Embedding, error)
}

// Repository adds the write capabilities required by semantic synchronization.
type Repository interface {
	ReadRepository
	UpsertEmbedding(context.Context, Embedding) error
	DeleteStaleEmbeddings(context.Context, string) (int64, error)
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
	repository Repository
	graph      graph.QueryRepository
	embedder   Embedder
	batchSize  int
	force      bool
	mu         sync.Mutex
}

func NewService(repository Repository, graphRepository graph.QueryRepository, embedder Embedder) *Service {
	return &Service{repository: repository, graph: graphRepository, embedder: embedder, batchSize: 32}
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
	model := strings.TrimSpace(s.embedder.Model())
	if model == "" {
		return SyncReport{}, errors.New("embedding model is required")
	}
	nodes, err := s.repository.CandidateNodes(ctx)
	if err != nil {
		return SyncReport{}, fmt.Errorf("list semantic candidates: %w", err)
	}
	hashes, err := s.repository.EmbeddingHashes(ctx, model)
	if err != nil {
		return SyncReport{}, fmt.Errorf("list embedding hashes: %w", err)
	}
	type pendingDocument struct {
		node graph.Node
		text string
		hash string
	}
	pending := make([]pendingDocument, 0)
	report := SyncReport{Model: model, Candidates: len(nodes)}
	for _, node := range nodes {
		text := Document(node)
		hash := contentHash(DocumentVersion + "\x00" + text)
		if !s.force && hashes[node.ID] == hash {
			report.Unchanged++
			continue
		}
		pending = append(pending, pendingDocument{node: node, text: text, hash: hash})
	}
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
		for offset, vector := range vectors {
			vector, err = normalized(vector)
			if err != nil {
				return report, fmt.Errorf("embed %s: %w", pending[start+offset].node.QualifiedName, err)
			}
			document := pending[start+offset]
			if err := s.repository.UpsertEmbedding(ctx, Embedding{NodeID: document.node.ID, Model: model,
				ContentHash: document.hash, Vector: vector, UpdatedAt: graph.NowUTC()}); err != nil {
				return report, fmt.Errorf("store embedding for %s: %w", document.node.QualifiedName, err)
			}
			report.Updated++
		}
	}
	report.Removed, err = s.repository.DeleteStaleEmbeddings(ctx, model)
	if err != nil {
		return report, fmt.Errorf("remove stale embeddings: %w", err)
	}
	return report, nil
}

func (s *Service) Search(ctx context.Context, text string, limit int) (SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embedder == nil {
		return SearchResult{}, errors.New("semantic embedder is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return SearchResult{}, errors.New("semantic query is required")
	}
	if limit <= 0 {
		limit = 5
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
	embeddings, err := s.repository.Embeddings(ctx, s.embedder.Model())
	if err != nil {
		return SearchResult{}, err
	}
	if len(embeddings) == 0 {
		return SearchResult{}, fmt.Errorf("no embeddings for model %q; run semantic sync first", s.embedder.Model())
	}
	type scoredNode struct {
		node  graph.Node
		score float64
	}
	scored := make([]scoredNode, 0, len(embeddings))
	for _, embedding := range embeddings {
		if len(embedding.Vector) != len(queryVector) {
			return SearchResult{}, fmt.Errorf("embedding dimensions changed for model %q: stored %d, query %d", s.embedder.Model(), len(embedding.Vector), len(queryVector))
		}
		node, err := s.graph.Node(ctx, embedding.NodeID)
		if err != nil {
			continue
		}
		scored = append(scored, scoredNode{node: node, score: dot(queryVector, embedding.Vector)})
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
	result := SearchResult{Query: text, Model: s.embedder.Model(), Matches: make([]Match, 0, len(scored))}
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
