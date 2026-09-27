package graph

import (
	"context"
	"time"
)

// IndexRepository is the narrow persistence port used by the indexing use case.
type IndexRepository interface {
	Meta(context.Context, string) (string, error)
	SetMeta(context.Context, string, string) error
	Files(context.Context) (map[string]FileRecord, error)
	ReplaceFile(context.Context, FileRecord, ParseResult) error
	ReplaceOwner(context.Context, string, ParseResult) error
	RemoveFiles(context.Context, []string) error
	Reconcile(context.Context) error
	Counts(context.Context) (Counts, error)
}

// ReconciliationStats describes durable work completed during reconciliation.
type ReconciliationStats struct {
	Batches int `json:"batches"`
}

// ReconciliationObserver runs after a reconciliation batch commits and before
// its WAL checkpoint. Returning an error stops at that durable boundary.
type ReconciliationObserver func(ReconciliationStats) error

// InstrumentedIndexRepository optionally exposes reconciliation progress.
// Indexers retain compatibility with repositories that implement only
// IndexRepository.
type InstrumentedIndexRepository interface {
	ReconcileWithStats(context.Context, ReconciliationObserver) (ReconciliationStats, error)
}

// QueryRepository is the read-only port used by graph traversal use cases.
type QueryRepository interface {
	SearchNodes(context.Context, string, int) ([]Node, error)
	Node(context.Context, string) (Node, error)
	EdgesFrom(context.Context, string) ([]Edge, error)
	EdgesTo(context.Context, string) ([]Edge, error)
}

// ExternalEdgeRepository exposes unresolved edge targets for cross-repository
// federation. It remains separate so traversal use cases do not require it.
type ExternalEdgeRepository interface {
	ExternalEdgesTo(context.Context, Node) ([]Edge, error)
}

// StatusRepository exposes only metadata and aggregate counts.
type StatusRepository interface {
	Meta(context.Context, string) (string, error)
	Counts(context.Context) (Counts, error)
}

type ReadRepository interface {
	QueryRepository
	StatusRepository
}

// Repository is the complete port implemented by a storage adapter. Services
// accept the smaller interfaces above so their dependencies stay explicit.
type Repository interface {
	IndexRepository
	QueryRepository
	StatusRepository
	Close() error
	Path() string
}

type FileRecord struct {
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Language   string `json:"language"`
	Size       int64  `json:"size"`
	ModifiedNS int64  `json:"modified_ns"`
	IndexedAt  string `json:"indexed_at"`
}

type Counts struct {
	Files    int            `json:"files"`
	Nodes    int            `json:"nodes"`
	Facts    int            `json:"facts"`
	Edges    int            `json:"edges"`
	External int            `json:"external_nodes"`
	ByKind   map[string]int `json:"nodes_by_kind"`
	ByEdge   map[string]int `json:"edges_by_kind"`
}

func NowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
