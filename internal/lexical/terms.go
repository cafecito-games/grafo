// Package lexical implements language-neutral declaration retrieval terms.
package lexical

import (
	"sort"
	"strings"
	"unicode"
)

// Terms splits punctuation, camel case, acronyms, and letter/digit boundaries.
func Terms(text string) []string {
	r := []rune(text)
	start := 0
	seen := map[string]bool{}
	add := func(end int) {
		if end > start {
			seen[strings.ToLower(string(r[start:end]))] = true
		}
	}
	for i, c := range r {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			add(i)
			start = i + 1
			continue
		}
		if i <= start {
			continue
		}
		p := r[i-1]
		boundary := unicode.IsUpper(c) && unicode.IsLower(p) || unicode.IsDigit(c) != unicode.IsDigit(p)
		if unicode.IsUpper(c) && unicode.IsUpper(p) && i+1 < len(r) && unicode.IsLower(r[i+1]) {
			boundary = true
		}
		if boundary {
			add(i)
			start = i
		}
	}
	add(len(r))
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}
