package query

import (
	"fmt"
	"sort"

	"github.com/cafecito-games/grafo/internal/pathscope"
)

// NormalizePathPrefixes validates, deduplicates, and orders repository-relative
// segment prefixes for CLI, MCP, and query callers.
func NormalizePathPrefixes(values []string) ([]string, error) {
	set := map[string]bool{}
	for _, value := range values {
		normalized, err := pathscope.NormalizePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid path prefix %q: %w", value, err)
		}
		set[normalized] = true
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizePathPrefixes(values []string) ([]string, error) { return NormalizePathPrefixes(values) }

func matchesPathPrefixes(candidate string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if pathscope.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}
