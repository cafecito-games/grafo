package graph

import (
	"context"
	"time"
)

// IndexRepository is the narrow persistence port used by the indexing use case.
type IndexRepository interface {
	SetMeta(context.Context, string, string) error
	Files(context.Context) (map[string]FileRecord, error)
	ReplaceFile(context.Context, FileRecord, ParseResult) error
	ReplaceOwner(context.Context, string, ParseResult) error
	RemoveFiles(context.Context, []string) error
	Reconcile(context.Context) error
	Counts(context.Context) (Counts, error)
}

// QueryRepository is the read-only port used by graph traversal use cases.
type QueryRepository interface {
	SearchNodes(context.Context, string, int) ([]Node, error)
	Node(context.Context, string) (Node, error)
	EdgesFrom(context.Context, string) ([]Edge, error)
	EdgesTo(context.Context, string) ([]Edge, error)
}

// StatusRepository exposes only metadata and aggregate counts.
type StatusRepository interface {
	Meta(context.Context, string) (string, error)
	Counts(context.Context) (Counts, error)
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
	Edges    int            `json:"edges"`
	External int            `json:"external_nodes"`
	ByKind   map[string]int `json:"nodes_by_kind"`
	ByEdge   map[string]int `json:"edges_by_kind"`
}

func NowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
