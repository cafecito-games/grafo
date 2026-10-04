package graph

import (
	"context"
	"fmt"
	"strings"
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

// FileReplacement is one file's record paired with the evidence parsed from it.
type FileReplacement struct {
	File   FileRecord
	Parsed ParseResult
}

// BulkIndexRepository optionally replaces several files in one durable step.
// Indexers fall back to ReplaceFile per file when a repository does not offer
// it, so the capability only ever changes how many files share a commit, never
// which evidence is stored.
type BulkIndexRepository interface {
	ReplaceFiles(context.Context, []FileReplacement) error
}

// BulkLoadRepository optionally reorganizes storage around an initial load of a
// whole repository. Indexers announce the boundaries; whether anything is
// reorganized, and what, is the adapter's decision, and a repository that does
// not offer the capability indexes exactly as before.
type BulkLoadRepository interface {
	BeginBulkLoad(context.Context) error
	EndBulkLoad(context.Context) error
}

// FileRecordRepository optionally records a file's inputs without rewriting the
// evidence it already contributed. A file can be reparsed because its inputs
// changed and still produce the evidence the index holds; this lets the indexer
// record why it was selected without reproducing rows that are already there,
// and without enqueueing reconciliation work for them.
//
// A repository that does not offer it has its evidence rewritten exactly as
// before, so the capability changes how much is written, never what the graph
// ends up containing.
type FileRecordRepository interface {
	UpdateFileRecord(context.Context, FileRecord) error
}

// ReconciliationStats describes durable work completed during reconciliation.
type ReconciliationStats struct {
	Batches int        `json:"batches"`
	Writes  WriteStats `json:"writes"`
}

// WriteBatchStats describes bounded adapter writes without exposing a storage
// dialect or its configured limits to indexing callers.
type WriteBatchStats struct {
	Batches int64 `json:"batches"`
	Rows    int64 `json:"rows"`
	Bytes   int64 `json:"bytes"`
}

// WriteStats reports rows and encoded payload bytes sent through bounded
// write batches. Adapters that do not batch writes may leave it empty.
type WriteStats struct {
	Nodes WriteBatchStats `json:"nodes"`
	Facts WriteBatchStats `json:"facts"`
	Edges WriteBatchStats `json:"edges"`
}

// ReconciliationObserver runs immediately after a reconciliation batch commits,
// before any WAL checkpoint that batch triggers. Returning an error stops at
// that durable boundary.
type ReconciliationObserver func(ReconciliationStats) error

// InstrumentedIndexRepository optionally exposes reconciliation progress.
// Indexers retain compatibility with repositories that implement only
// IndexRepository.
type InstrumentedIndexRepository interface {
	ReconcileWithStats(context.Context, ReconciliationObserver) (ReconciliationStats, error)
}

// InstrumentedWriteRepository optionally exposes cumulative bounded-write
// metrics. Indexers take a per-run delta so a repository can be reused.
type InstrumentedWriteRepository interface {
	WriteStats() WriteStats
}

// ReconciliationStatusRepository is the optional, narrow capability used by
// indexing to prove that no resolver work remains. Absence of this capability
// is deliberately treated as unknown, never as clean.
type ReconciliationStatusRepository interface {
	ReconciliationPending(context.Context) (bool, error)
}

// QueryRepository is the read-only port used by graph traversal use cases.
type QueryRepository interface {
	SearchNodes(context.Context, string, int) ([]Node, error)
	// MatchNodes returns the strongest non-empty match evidence for a selector.
	// Selector resolution depends on its contract; see NodeMatchGroup.
	MatchNodes(context.Context, NodeMatchQuery) (NodeMatchGroup, error)
	Node(context.Context, string) (Node, error)
	EdgesFrom(context.Context, string) ([]Edge, error)
	EdgesTo(context.Context, string) ([]Edge, error)
}

// ExternalEdgeRepository exposes unresolved edge targets for cross-repository
// federation. It remains separate so traversal use cases do not require it.
type ExternalEdgeRepository interface {
	ExternalEdgesTo(context.Context, Node) ([]Edge, error)
}

// ExternalNodeRepository enumerates unresolved boundary nodes that a declared
// node can replace during federation. It returns exact name equivalence only;
// relation evidence is still loaded exclusively through RelationEdgeRepository.
type ExternalNodeRepository interface {
	ExternalNodesMatching(context.Context, Node) ([]Node, error)
}

// ExternalRequestEdge is one persisted request boundary plus both endpoint and
// source nodes. Federation consumes these in bounded pages to build a symmetric
// route projection without per-endpoint adjacency queries.
type ExternalRequestEdge struct {
	Edge   Edge
	Source Node
	Target Node
}

// ExternalRequestEdgeCursor positions a page within the external request edges.
// It is the three columns an edge's identity is derived from rather than the
// identity itself: storage no longer stores the identity, so the ordering and the
// cursor are both expressed in the columns that determine it.
type ExternalRequestEdgeCursor struct {
	FactID string
	ToID   string
	Kind   EdgeKind
}

// Compare orders two cursors the way the query that produces them orders rows:
// by fact_id, then to_id, then kind. It has to mirror that ordering exactly,
// because a caller uses it to prove a page advanced, and a comparison that
// disagreed with the SQL would either reject a valid page or loop forever on an
// invalid one.
func (c ExternalRequestEdgeCursor) Compare(other ExternalRequestEdgeCursor) int {
	if c.FactID != other.FactID {
		return strings.Compare(c.FactID, other.FactID)
	}
	if c.ToID != other.ToID {
		return strings.Compare(c.ToID, other.ToID)
	}
	return strings.Compare(string(c.Kind), string(other.Kind))
}

type ExternalRequestEdgePage struct {
	Items []ExternalRequestEdge
	// Next is nil when the page is the last one, so a caller loops until it is
	// rather than comparing against an empty cursor that is also a valid start.
	Next *ExternalRequestEdgeCursor
}

type ExternalRequestEdgeRepository interface {
	// A nil cursor starts at the first page.
	ExternalRequestEdges(context.Context, *ExternalRequestEdgeCursor, int) (ExternalRequestEdgePage, error)
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
	// PathPrefixes are normalized repository-relative segment prefixes. They
	// filter canonical node locations before the per-kind Limit is applied.
	PathPrefixes []string
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

// CanonicalMessageQuery selects locally declared protocol-message types before
// applying its bound. Package and Message are exact semantic filters rather
// than post-enumeration presentation filters.
type CanonicalMessageQuery struct {
	Repository   string
	Package      string
	Message      string
	PathPrefixes []string
	Limit        int
}

func (q CanonicalMessageQuery) Validate() error {
	if q.Limit <= 0 {
		return fmt.Errorf("canonical message limit must be positive")
	}
	return nil
}

// CanonicalMessagePage reports whether more matching canonical messages exist
// beyond the requested bound.
type CanonicalMessagePage struct {
	Items     []ScopedNode
	Truncated bool
}

// CanonicalMessageRepository is the narrow bounded enumeration port used by
// message coverage. Implementations filter authoritative message declarations
// before sorting and limiting.
type CanonicalMessageRepository interface {
	CanonicalMessages(context.Context, CanonicalMessageQuery) (CanonicalMessagePage, error)
}

// RelationDirection selects which side of a subject node a bounded catalog
// evidence lookup follows.
type RelationDirection string

const (
	IncomingRelations RelationDirection = "incoming"
	OutgoingRelations RelationDirection = "outgoing"
)

// RelationEdgeQuery requests exact relations adjacent to one subject. Limit is
// applied independently to each distinct relation.
type RelationEdgeQuery struct {
	SubjectID string
	Direction RelationDirection
	Relations []EdgeKind
	Limit     int
}

func (q RelationEdgeQuery) Validate() error {
	if strings.TrimSpace(q.SubjectID) == "" {
		return fmt.Errorf("relation edge subject is required")
	}
	if q.Direction != IncomingRelations && q.Direction != OutgoingRelations {
		return fmt.Errorf("unknown relation direction %q", q.Direction)
	}
	if len(q.Relations) == 0 {
		return fmt.Errorf("at least one exact relation is required")
	}
	for _, relation := range q.Relations {
		if relation == "" {
			return fmt.Errorf("relation must not be empty")
		}
	}
	if q.Limit <= 0 {
		return fmt.Errorf("relation edge limit must be positive")
	}
	return nil
}

// HydratedRelationEdge pairs an edge with the node opposite the request's
// subject. Adapters return the pair atomically so catalogs never perform an
// N+1 node lookup or silently omit a missing counterpart.
type HydratedRelationEdge struct {
	Edge        Edge
	Counterpart Node
	// Repository identifies the indexed repository that owns Counterpart.
	// Single-repository callers may leave it empty and provide their known scope.
	Repository string
}

// RelationEdgePage contains at most Limit edges per requested relation.
type RelationEdgePage struct {
	Items     []HydratedRelationEdge
	Truncated bool
}

// RelationEdgeRepository is the bounded, relation-filtered evidence port used
// by catalogs. Traversal continues to use QueryRepository adjacency methods.
type RelationEdgeRepository interface {
	RelationEdges(context.Context, RelationEdgeQuery) (RelationEdgePage, error)
}

// CatalogRepository is the read port the data, configuration, and event
// catalogs depend on.
type CatalogRepository interface {
	Node(context.Context, string) (Node, error)
	NodeListRepository
	RelationEdgeRepository
}

// TopologyRepository adds traversal adjacency to the bounded catalog port.
// Catalog-only consumers do not inherit unbounded traversal methods.
type TopologyRepository interface {
	CatalogRepository
	QueryRepository
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
	// EvidenceDigest is what this file's nodes and facts digested to when they
	// were last written. An empty value means no digest is recorded, which no
	// real digest can equal, so the evidence is written rather than elided.
	EvidenceDigest string `json:"evidence_digest,omitempty"`
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

// FileCatalog exposes the indexed file records of one repository index. It is
// the authority for search membership; search never walks the filesystem.
type FileCatalog interface {
	Files(context.Context) (map[string]FileRecord, error)
}
