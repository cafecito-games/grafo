// Package investigation assembles bounded discovery evidence, not generated answers.
package investigation

import (
	"github.com/cafecito-games/grafo/internal/graph"
)

type Request struct {
	Query          string   `json:"query" jsonschema:"Natural-language question or symbol terms; 1 to 2000 bytes; domain synonyms are not expanded."`
	Repository     string   `json:"repository,omitempty" jsonschema:"Exact indexed repository name."`
	Component      string   `json:"component,omitempty" jsonschema:"Exact indexed component; retrieval uses recorded ownership."`
	PathPrefixes   []string `json:"path_prefixes,omitempty" jsonschema:"Repository-relative segment prefixes; restricts source and related evidence."`
	Languages      []string `json:"languages,omitempty" jsonschema:"Exact parser language identifiers."`
	MaxCandidates  int      `json:"max_candidates,omitempty" jsonschema:"Candidate count; default 5; range 1 to 12."`
	MaxSourceLines int      `json:"max_source_lines,omitempty" jsonschema:"Global returned source lines; default 240; range 1 to 600."`
	MaxSourceBytes int      `json:"max_source_bytes,omitempty" jsonschema:"Global bytes read from source files; default 524288; range 1 to 2097152."`
	MaxOutputBytes int      `json:"max_output_bytes,omitempty" jsonschema:"Canonical JSON response bytes; default 24000; range 2000 to 24000."`
	MaxWork        int      `json:"max_work,omitempty" jsonschema:"Global postings, hydrated declarations, and graph work units; default 12000; range 32 to 50000."`
	TimeoutMS      int      `json:"timeout_ms,omitempty" jsonschema:"Request deadline in milliseconds; default 4000; range 1 to 8000."`
}
type Candidate struct {
	NodeID       string   `json:"node_id"`
	Repository   string   `json:"repository,omitempty"`
	Score        int      `json:"score"`
	MatchedTerms []string `json:"matched_terms"`
	Designation  string   `json:"designation"`
}
type Node struct {
	ID         string         `json:"id"`
	Repository string         `json:"repository,omitempty"`
	Name       string         `json:"name"`
	Kind       graph.NodeKind `json:"kind"`
	Path       string         `json:"path,omitempty"`
	Line       int            `json:"line,omitempty"`
	EndLine    int            `json:"end_line,omitempty"`
}
type Edge struct {
	Repository string         `json:"repository,omitempty"`
	ID         string         `json:"id"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	Kind       graph.EdgeKind `json:"kind"`
	Path       string         `json:"path,omitempty"`
	Line       int            `json:"line,omitempty"`
}
type Excerpt struct {
	NodeID     string `json:"node_id"`
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated,omitempty"`
}
type Followup struct {
	Repository string `json:"repository,omitempty"`
	Tool       string `json:"tool"`
	Selector   string `json:"selector,omitempty"`
	Reason     string `json:"reason"`
}
type Usage struct {
	Work          int `json:"work"`
	Postings      int `json:"postings"`
	Declarations  int `json:"declarations"`
	MetadataReads int `json:"metadata_reads"`
	GraphQueries  int `json:"graph_queries"`
	SourceBytes   int `json:"source_bytes"`
	SourceLines   int `json:"source_lines"`
	OutputBytes   int `json:"output_bytes"`
}
type Report struct {
	Query      string      `json:"query"`
	Status     string      `json:"status"`
	Candidates []Candidate `json:"candidates"`
	Nodes      []Node      `json:"nodes"`
	Edges      []Edge      `json:"edges"`
	Excerpts   []Excerpt   `json:"excerpts"`
	Boundaries []string    `json:"boundaries"`
	Followups  []Followup  `json:"followups"`
	Usage      Usage       `json:"usage"`
	Truncated  bool        `json:"truncated"`
}
