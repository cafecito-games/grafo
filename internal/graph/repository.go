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

// NodeVisibility selects whether an enumeration returns locally declared
// nodes, unresolved external nodes, or both.
type NodeVisibility string

const (
	LocalNodes    NodeVisibility = "local"
	ExternalNodes NodeVisibility = "external"
	AllNodes      NodeVisibility = "all"
)

// NodeListQuery selects nodes for catalog use cases. It expresses enumeration
// only: exact kinds, an optional name fragment, and explicit bounds.
type NodeListQuery struct {
	Kinds      []NodeKind
	Name       string
	Repository string
	// Visibility defaults to LocalNodes so a catalog never silently mixes
	// declarations with unresolved external targets.
	Visibility NodeVisibility
	// Limit bounds the rows returned for each requested kind. Callers that
	// merge several kinds apply their own total bound and report truncation.
	Limit int
}

// ScopedNode pairs a node with the indexed repository that stores it.
type ScopedNode struct {
	Repository string `json:"repository,omitempty"`
	Node       Node   `json:"node"`
}

// NodeListRepository enumerates nodes by exact kind and attributes them to an
// indexed repository. Adapters only enumerate; catalog semantics such as usage
// direction and orphan classification stay in the query use cases.
type NodeListRepository interface {
	Repositories(context.Context) ([]string, error)
	ListNodesByKind(context.Context, NodeListQuery) ([]ScopedNode, error)
}

// CatalogRepository is the read port the data, configuration, and event
// catalogs depend on.
type CatalogRepository interface {
	QueryRepository
	NodeListRepository
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
