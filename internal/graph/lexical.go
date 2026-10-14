package graph

import "context"

// DeclarationQuery bounds postings visited and distinct declarations hydrated.
// Scope is applied before ranking; exhausted postings may leave scope incomplete.
const DefaultDeclarationMetadata = 64

type DeclarationQuery struct {
	Terms         []string
	Repository    string
	Component     string
	PathPrefixes  []string
	Languages     []string
	Limit         int
	MaxPostings   int
	MaxCandidates int
	// MaxMetadata bounds readiness, repository, and term-population probes.
	// Zero selects DefaultDeclarationMetadata.
	MaxMetadata int
}
type DeclarationMatch struct {
	Node            Node
	Repository      string
	Score           int
	MatchedTerms    []string
	NameTerms       []string
	Generated       bool
	InterfaceMember bool
}

// DeclarationPage counts every returned posting projection, including duplicates,
// and every complete node hydrated, including rejected component candidates.
// Lightweight projections are bounded by MaxPostings and ranked after path and
// language filtering, before MaxCandidates limits complete node hydration. Each
// hydration performs bounded metadata probes (at most 2 file nodes, 33 parent
// edges, and 33 component-parent edges). Truncated covers both visit exhaustion
// and omitted ranked results; it never means the repository has no other matches.
type DeclarationPage struct {
	Matches        []DeclarationMatch
	PostingsRead   int
	CandidatesRead int
	MetadataReads  int
	Truncated      bool
}
type DeclarationRepository interface {
	SearchDeclarations(context.Context, DeclarationQuery) (DeclarationPage, error)
}
