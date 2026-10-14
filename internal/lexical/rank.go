package lexical

import (
	"github.com/cafecito-games/grafo/internal/graph"
	"slices"
	"sort"
)

// Rank scores retrieval relevance only, never graph evidence or confidence.
func Rank(n graph.Node, terms []string) graph.DeclarationMatch {
	m := graph.DeclarationMatch{Node: n, NameTerms: Terms(n.Name)}
	qualified, path := Terms(n.QualifiedName), Terms(n.Location.Path)
	nameMatches := 0
	for _, t := range terms {
		score := 0
		switch {
		case slices.Contains(m.NameTerms, t):
			score = 100
			nameMatches++
		case slices.Contains(qualified, t):
			score = 35
		case slices.Contains(path, t):
			score = 15
		}
		if score > 0 {
			m.Score += score
			m.MatchedTerms = append(m.MatchedTerms, t)
		}
	}
	m.Score += 80 * len(m.MatchedTerms) * len(m.MatchedTerms)
	// Prefer a focused declaration name over incidental matching words in a long name.
	if len(m.NameTerms) > 0 {
		m.Score += 60 * nameMatches / len(m.NameTerms)
	}
	return m
}

// Sort orders matches deterministically, including across repositories.
func Sort(matches []graph.DeclarationMatch) {
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Node.QualifiedName != b.Node.QualifiedName {
			return a.Node.QualifiedName < b.Node.QualifiedName
		}
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		return a.Node.ID < b.Node.ID
	})
}
