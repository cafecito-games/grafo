package search

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// fakeCatalog is a graph.FileCatalog backed by an in-memory map.
type fakeCatalog struct {
	files map[string]graph.FileRecord
	err   error
}

func (c fakeCatalog) Files(context.Context) (map[string]graph.FileRecord, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.files, nil
}

// worktree writes files under a temporary root and returns a Source whose
// catalog lists exactly the given paths.
func worktree(t *testing.T, name string, files map[string]string, languages map[string]string) Source {
	t.Helper()
	root := testtemp.Dir(t)
	records := map[string]graph.FileRecord{}
	for path, content := range files {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		records[path] = graph.FileRecord{Path: path, Language: languages[path], Size: info.Size()}
	}
	return Source{
		Project: indexer.Project{Root: root, Name: name, Branch: "main"},
		Catalog: fakeCatalog{files: records},
	}
}

func catalogOnly(source Source, extra map[string]graph.FileRecord) Source {
	records := map[string]graph.FileRecord{}
	for path, record := range source.Catalog.(fakeCatalog).files {
		records[path] = record
	}
	for path, record := range extra {
		records[path] = record
	}
	source.Catalog = fakeCatalog{files: records}
	return source
}

const sampleGo = "package alpha\n\nfunc Alpha() {\n\treturn\n}\n\nfunc alphaHelper() {\n\treturn\n}\n"

func sampleSource(t *testing.T) Source {
	t.Helper()
	return worktree(t, "repo", map[string]string{
		"internal/alpha.go": sampleGo,
		"internal/beta.py":  "def beta():\n    alpha = 1\n    return alpha\n",
		"docs/notes.md":     "# Alpha notes\n\nalpha appears here.\n",
		"vendor/ignored.go": "package vendor\n\n// alpha in vendor\n",
	}, map[string]string{
		"internal/alpha.go": "go",
		"internal/beta.py":  "python",
		"docs/notes.md":     "markdown",
		"vendor/ignored.go": "go",
	})
}

func TestSearchMatching(t *testing.T) {
	source := sampleSource(t)
	tests := []struct {
		name          string
		request       Request
		wantPaths     []string
		wantCount     int
		wantFirstLine int
		wantFirstCol  int
	}{
		{
			name:      "literal case insensitive",
			request:   Request{Patterns: []string{"alpha"}},
			wantCount: 8,
		},
		{
			name:          "literal case sensitive",
			request:       Request{Patterns: []string{"Alpha"}, CaseSensitive: true},
			wantCount:     2,
			wantFirstLine: 1,
			wantFirstCol:  3,
		},
		{
			name:      "regex anchored",
			request:   Request{Patterns: []string{`^func \w+\(\)`}, Regex: true, CaseSensitive: true},
			wantCount: 2,
			wantPaths: []string{"internal/alpha.go"},
		},
		{
			name:      "regex case insensitive",
			request:   Request{Patterns: []string{`^ALPHA`}, Regex: true},
			wantCount: 1,
			wantPaths: []string{"docs/notes.md"},
		},
		{
			name:      "path prefix filter",
			request:   Request{Patterns: []string{"alpha"}, PathPrefixes: []string{"internal"}},
			wantPaths: []string{"internal/alpha.go", "internal/beta.py"},
		},
		{
			name:      "language filter",
			request:   Request{Patterns: []string{"alpha"}, Languages: []string{"Markdown"}},
			wantPaths: []string{"docs/notes.md"},
			wantCount: 2,
		},
		{
			name:      "repository filter excludes everything",
			request:   Request{Patterns: []string{"alpha"}, Repositories: []string{"other"}},
			wantCount: 0,
		},
		{
			name:      "repository filter matches",
			request:   Request{Patterns: []string{"alpha"}, Repositories: []string{"REPO"}},
			wantCount: 8,
		},
		{
			name:      "no matches",
			request:   Request{Patterns: []string{"zeta-not-present"}},
			wantCount: 0,
		},
		{
			name:      "multiple patterns",
			request:   Request{Patterns: []string{"beta", "vendor"}},
			wantCount: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := NewService([]Source{source}).Search(context.Background(), test.request)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if test.wantCount > 0 && len(result.Matches) != test.wantCount {
				t.Fatalf("got %d matches, want %d: %+v", len(result.Matches), test.wantCount, result.Matches)
			}
			if test.wantCount == 0 && test.wantPaths == nil && len(result.Matches) != 0 {
				t.Fatalf("expected no matches, got %+v", result.Matches)
			}
			if test.wantPaths != nil {
				seen := map[string]bool{}
				for _, match := range result.Matches {
					seen[match.Path] = true
				}
				if len(seen) != len(test.wantPaths) {
					t.Fatalf("got paths %v, want %v", sortedKeys(seen), test.wantPaths)
				}
				for _, path := range test.wantPaths {
					if !seen[path] {
						t.Fatalf("missing path %s in %v", path, sortedKeys(seen))
					}
				}
			}
			if test.wantFirstLine > 0 {
				first := result.Matches[0]
				if first.Line != test.wantFirstLine || first.Column != test.wantFirstCol {
					t.Fatalf("got %d:%d, want %d:%d", first.Line, first.Column, test.wantFirstLine, test.wantFirstCol)
				}
			}
			if result.Truncated {
				t.Fatalf("unexpected truncation: %v", result.Notes)
			}
		})
	}
}

func TestSearchMatchMetadata(t *testing.T) {
	source := sampleSource(t)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alphaHelper"}, CaseSensitive: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(result.Matches))
	}
	match := result.Matches[0]
	if match.Repository != "repo" || match.Branch != "main" {
		t.Fatalf("unexpected repository metadata: %+v", match)
	}
	if match.Path != "internal/alpha.go" || match.Language != "go" {
		t.Fatalf("unexpected path metadata: %+v", match)
	}
	if match.Line != 7 || match.Column != 6 {
		t.Fatalf("got %d:%d, want 7:6", match.Line, match.Column)
	}
	if match.Text != "func alphaHelper() {" {
		t.Fatalf("unexpected text %q", match.Text)
	}
	if result.FilesSearched != 4 || result.FilesSkipped != 0 {
		t.Fatalf("got searched=%d skipped=%d", result.FilesSearched, result.FilesSkipped)
	}
}

func TestSearchContextLines(t *testing.T) {
	source := worktree(t, "repo", map[string]string{
		"a.txt": "one\ntwo\nthree\nfour\nfive\n",
	}, nil)
	tests := []struct {
		name       string
		pattern    string
		lines      int
		wantBefore []string
		wantAfter  []string
	}{
		{name: "middle", pattern: "three", lines: 2,
			wantBefore: []string{"one", "two"}, wantAfter: []string{"four", "five"}},
		{name: "file start", pattern: "one", lines: 2,
			wantBefore: nil, wantAfter: []string{"two", "three"}},
		{name: "file end", pattern: "five", lines: 2,
			wantBefore: []string{"three", "four"}, wantAfter: nil},
		{name: "zero context", pattern: "three", lines: 0,
			wantBefore: nil, wantAfter: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := NewService([]Source{source}).Search(context.Background(),
				Request{Patterns: []string{test.pattern}, ContextLines: test.lines})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(result.Matches) != 1 {
				t.Fatalf("got %d matches, want 1", len(result.Matches))
			}
			if strings.Join(result.Matches[0].Before, "|") != strings.Join(test.wantBefore, "|") {
				t.Fatalf("before = %v, want %v", result.Matches[0].Before, test.wantBefore)
			}
			if strings.Join(result.Matches[0].After, "|") != strings.Join(test.wantAfter, "|") {
				t.Fatalf("after = %v, want %v", result.Matches[0].After, test.wantAfter)
			}
		})
	}
}

func TestSearchCaps(t *testing.T) {
	source := worktree(t, "repo", map[string]string{
		"a.txt": "hit hit hit\nhit hit hit\n",
		"b.txt": "hit hit hit\nhit hit hit\n",
	}, nil)
	tests := []struct {
		name      string
		request   Request
		wantCount int
		wantNote  string
	}{
		{
			name:      "per file cap",
			request:   Request{Patterns: []string{"hit"}, MaxMatchesPerFile: 2},
			wantCount: 4,
			wantNote:  "max_matches_per_file cap of 2 clipped matches in 2 file(s): repo/a.txt, repo/b.txt",
		},
		{
			name:      "per pattern cap",
			request:   Request{Patterns: []string{"hit"}, MaxMatchesPattern: 3},
			wantCount: 3,
			wantNote:  `max_matches_pattern cap of 3 clipped matches for pattern "hit"`,
		},
		{
			name:      "total cap",
			request:   Request{Patterns: []string{"hit"}, MaxMatches: 5},
			wantCount: 5,
			wantNote:  "max_matches cap of 5 reached; results are truncated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := NewService([]Source{source}).Search(context.Background(), test.request)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(result.Matches) != test.wantCount {
				t.Fatalf("got %d matches, want %d", len(result.Matches), test.wantCount)
			}
			if !result.Truncated {
				t.Fatal("expected Truncated")
			}
			if !hasNote(result.Notes, test.wantNote) {
				t.Fatalf("notes %v missing %q", result.Notes, test.wantNote)
			}
		})
	}
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if note == want {
			return true
		}
	}
	return false
}

func TestSearchSkipsBinaryAndOversized(t *testing.T) {
	source := worktree(t, "repo", map[string]string{
		"binary.bin": "prefix\x00\x01binary alpha",
		"big.txt":    strings.Repeat("alpha padding line\n", 200),
		"small.txt":  "alpha here\n",
	}, nil)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alpha"}, MaxFileBytes: 64})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Path != "small.txt" {
		t.Fatalf("unexpected matches %+v", result.Matches)
	}
	if result.FilesSearched != 1 || result.FilesSkipped != 2 {
		t.Fatalf("got searched=%d skipped=%d", result.FilesSearched, result.FilesSkipped)
	}
	if !hasNote(result.Notes, "skipped binary content: 1 file(s) including repo/binary.bin") {
		t.Fatalf("notes %v missing binary skip", result.Notes)
	}
	if !hasNote(result.Notes, "skipped content exceeding max_file_bytes of 64: 1 file(s) including repo/big.txt") {
		t.Fatalf("notes %v missing oversize skip", result.Notes)
	}
}

func TestSearchSkipsMissingEscapingAndSymlinkedFiles(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"present.txt": "alpha\n"}, nil)
	outside := filepath.Join(testtemp.Dir(t), "secret.txt")
	if err := os.WriteFile(outside, []byte("alpha secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(source.Project.Root, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks unavailable")
		}
		t.Fatalf("symlink: %v", err)
	}
	source = catalogOnly(source, map[string]graph.FileRecord{
		"gone.txt":                {Path: "gone.txt"},
		"escape.txt":              {Path: "escape.txt"},
		"../outside.txt":          {Path: "../outside.txt"},
		filepath.ToSlash(outside): {Path: filepath.ToSlash(outside)},
	})
	result, err := NewService([]Source{source}).Search(context.Background(), Request{Patterns: []string{"alpha"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Path != "present.txt" {
		t.Fatalf("unexpected matches %+v", result.Matches)
	}
	if result.FilesSkipped != 4 {
		t.Fatalf("got skipped=%d, want 4", result.FilesSkipped)
	}
	for _, match := range result.Matches {
		if strings.Contains(match.Text, "secret") {
			t.Fatal("search followed a symlink outside the repository root")
		}
	}
	if !hasNote(result.Notes, "skipped unreadable or missing files: 1 file(s) including repo/gone.txt") {
		t.Fatalf("notes %v missing stale-file skip", result.Notes)
	}
	if !strings.Contains(strings.Join(result.Notes, "\n"), "skipped paths that resolve outside the repository root: 3 file(s)") {
		t.Fatalf("notes %v missing escape skip", result.Notes)
	}
}

func TestSearchPathPrefixRespectsSegmentBoundaries(t *testing.T) {
	source := worktree(t, "repo", map[string]string{
		"internal/alpha.go":     "alpha\n",
		"internal-old/alpha.go": "alpha\n",
	}, map[string]string{
		"internal/alpha.go":     "go",
		"internal-old/alpha.go": "go",
	})
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alpha"}, PathPrefixes: []string{"internal"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Path != "internal/alpha.go" {
		t.Fatalf("prefix internal must select internal/alpha.go only, got %+v", result.Matches)
	}
}

func TestSearchRejectsInvalidPathPrefixes(t *testing.T) {
	source := sampleSource(t)
	service := NewService([]Source{source})
	tests := []struct {
		name     string
		prefixes []string
		want     string
	}{
		{name: "traversal", prefixes: []string{"../etc"}, want: "traversal"},
		{name: "glob", prefixes: []string{"a*b"}, want: "glob"},
		{name: "backslash", prefixes: []string{`internal\app`}, want: "slash separators"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Search(context.Background(), Request{Patterns: []string{"alpha"}, PathPrefixes: test.prefixes})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not contain %q", err, test.want)
			}
		})
	}
}

func TestSearchRejectsInvalidRequests(t *testing.T) {
	source := sampleSource(t)
	service := NewService([]Source{source})
	tests := []struct {
		name    string
		request Request
		want    string
	}{
		{name: "no patterns", request: Request{}, want: "at least one search pattern"},
		{name: "empty pattern", request: Request{Patterns: []string{"  "}}, want: "must not be empty"},
		{name: "invalid regex", request: Request{Patterns: []string{"alpha", "func ("}, Regex: true},
			want: `invalid regular expression "func ("`},
		{name: "oversized pattern", request: Request{Patterns: []string{strings.Repeat("a", MaxPatternBytes+1)}},
			want: "exceeds 1024 bytes"},
		{name: "too many patterns", request: Request{Patterns: make([]string, MaxPatterns+1)},
			want: "at most 64 patterns"},
		{name: "context lines too high", request: Request{Patterns: []string{"alpha"}, ContextLines: 21},
			want: "context lines must be between 0 and 20"},
		{name: "negative context lines", request: Request{Patterns: []string{"alpha"}, ContextLines: -1},
			want: "context lines must be between 0 and 20"},
		{name: "negative total cap", request: Request{Patterns: []string{"alpha"}, MaxMatches: -1},
			want: "max matches must be between 1 and 500"},
		{name: "negative per-file cap", request: Request{Patterns: []string{"alpha"}, MaxMatchesPerFile: -2},
			want: "max matches per file must be between 1 and 50"},
		{name: "negative per-pattern cap", request: Request{Patterns: []string{"alpha"}, MaxMatchesPattern: -3},
			want: "max matches per pattern must be between 1 and 200"},
		{name: "file bytes too large", request: Request{Patterns: []string{"alpha"}, MaxFileBytes: MaxFileBytesCeiling + 1},
			want: "max file bytes must be between"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Search(context.Background(), test.request)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), test.want)
			}
		})
	}
}

func TestSearchInvalidRegexIsNotTreatedAsLiteral(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": "func (\n"}, nil)
	_, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"func ("}, Regex: true})
	if err == nil {
		t.Fatal("expected invalid regex error")
	}
	if !strings.Contains(err.Error(), "func (") {
		t.Fatalf("error %q does not name the pattern", err.Error())
	}
}

func TestSearchRequiresSources(t *testing.T) {
	_, err := NewService(nil).Search(context.Background(), Request{Patterns: []string{"alpha"}})
	if err == nil {
		t.Fatal("expected an error with no sources")
	}
}

func TestSearchPropagatesCatalogError(t *testing.T) {
	source := Source{Project: indexer.Project{Root: testtemp.Dir(t), Name: "repo"},
		Catalog: fakeCatalog{err: errors.New("catalog unavailable")}}
	_, err := NewService([]Source{source}).Search(context.Background(), Request{Patterns: []string{"alpha"}})
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("expected an error naming the repository, got %v", err)
	}
}

func TestSearchRespectsCancellation(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": strings.Repeat("alpha\n", 5000)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewService([]Source{source}).Search(ctx, Request{Patterns: []string{"alpha"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestSearchCancellationInsideLargeFile(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": strings.Repeat("alpha\n", 20000)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	service := NewService([]Source{source})
	// Cancellation is checked between files and periodically within a file;
	// with the context live the search completes under the total cap.
	result, err := service.Search(ctx, Request{Patterns: []string{"alpha"}, MaxMatches: 10, MaxMatchesPerFile: 10, MaxMatchesPattern: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 10 || !result.Truncated {
		t.Fatalf("unexpected result %+v", result)
	}
	cancel()
	if _, err := service.Search(ctx, Request{Patterns: []string{"alpha"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestSearchDeterministicOrderingAcrossRepositories(t *testing.T) {
	zulu := worktree(t, "zulu", map[string]string{
		"b.txt": "alpha\nalpha alpha\n",
		"a.txt": "alpha\n",
	}, nil)
	alpha := worktree(t, "alpha-repo", map[string]string{
		"z.txt": "alpha\n",
		"a.txt": "alpha alpha\n",
	}, nil)
	// Sources are supplied in a different order on each run.
	first, err := NewService([]Source{zulu, alpha}).Search(context.Background(),
		Request{Patterns: []string{"alpha", "alph"}, ContextLines: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	second, err := NewService([]Source{alpha, zulu}).Search(context.Background(),
		Request{Patterns: []string{"alph", "alpha"}, ContextLines: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("results differ:\n%s\n%s", firstJSON, secondJSON)
	}
	for index := 1; index < len(first.Matches); index++ {
		previous, current := first.Matches[index-1], first.Matches[index]
		if previous.Repository > current.Repository {
			t.Fatalf("repositories out of order at %d: %+v", index, first.Matches)
		}
		if previous.Repository == current.Repository && previous.Path > current.Path {
			t.Fatalf("paths out of order at %d: %+v", index, first.Matches)
		}
		if previous.Repository == current.Repository && previous.Path == current.Path {
			if previous.Line > current.Line ||
				(previous.Line == current.Line && previous.Column > current.Column) ||
				(previous.Line == current.Line && previous.Column == current.Column && previous.Pattern > current.Pattern) {
				t.Fatalf("matches out of order at %d: %+v", index, first.Matches)
			}
		}
	}
	if first.Matches[0].Repository != "alpha-repo" {
		t.Fatalf("expected alpha-repo first, got %s", first.Matches[0].Repository)
	}
}

func TestSearchNeverLeavesTheProvidedSources(t *testing.T) {
	included := worktree(t, "included", map[string]string{"a.txt": "alpha\n"}, nil)
	excluded := worktree(t, "excluded", map[string]string{"a.txt": "alpha\n"}, nil)
	result, err := NewService([]Source{included}).Search(context.Background(), Request{Patterns: []string{"alpha"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, match := range result.Matches {
		if match.Repository == excluded.Project.Name {
			t.Fatal("search reached a repository outside the configured sources")
		}
	}
	if len(result.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(result.Matches))
	}
}

func TestServiceSourcesAreCopied(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": "alpha\n"}, nil)
	service := NewService([]Source{source})
	sources := service.Sources()
	sources[0].Project.Name = "mutated"
	if service.Sources()[0].Project.Name != "repo" {
		t.Fatal("Sources returned an aliased slice")
	}
}

// TestSearchDefaultBoundsAreTheDocumentedOnes pins the defaults the CLI help,
// the MCP schema and the README all advertise. Raising them silently would
// restore the unbounded responses this bound exists to prevent.
func TestSearchDefaultBoundsAreTheDocumentedOnes(t *testing.T) {
	limits, err := resolveBounds(Request{Patterns: []string{"alpha"}})
	if err != nil {
		t.Fatalf("resolve bounds: %v", err)
	}
	want := bounds{perFile: 10, perPattern: 50, total: 100,
		fileBytes: 1 << 20, responseBytes: 32 << 10}
	if limits != want {
		t.Fatalf("default bounds %+v, want %+v", limits, want)
	}
}

func TestSearchBoundsResponseBytes(t *testing.T) {
	// Every count cap is satisfied: ten short lines, one match each, well
	// inside the per-file, per-pattern and total caps. Only the payload
	// budget can clip this response.
	line := strings.Repeat("x", 200) + "alpha\n"
	source := worktree(t, "repo", map[string]string{"a.txt": strings.Repeat(line, 10)}, nil)
	const budget = 600

	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alpha"}, MaxResponseBytes: budget})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.ResponseBytes > budget {
		t.Fatalf("response accounted %d bytes, over the %d budget", result.ResponseBytes, budget)
	}
	if len(result.Matches) == 0 || len(result.Matches) >= 10 {
		t.Fatalf("expected a clipped prefix of the ten matches, got %d", len(result.Matches))
	}
	if !result.Truncated {
		t.Fatalf("a clipped response must report truncation: %+v", result)
	}
	if !hasNote(result.Notes, "max_response_bytes budget of 600 reached; results are truncated") {
		t.Fatalf("notes %v missing the payload-budget note", result.Notes)
	}
	if !hasNote(result.Notes, truncationDisclaimer) {
		t.Fatalf("notes %v missing the truncation disclaimer", result.Notes)
	}
	// The retained matches are the deterministic prefix of the reporting
	// order, and the accounted bytes are the bytes actually returned.
	var accounted int64
	for index, match := range result.Matches {
		if match.Line != index+1 {
			t.Fatalf("match %d is line %d; the retained set must be the reporting prefix", index, match.Line)
		}
		accounted += matchBytes(match)
	}
	if accounted != result.ResponseBytes {
		t.Fatalf("reported %d bytes, matches account for %d", result.ResponseBytes, accounted)
	}
}

// TestSearchStopsReadingOnceTheBudgetIsSpent pins the cost side of the bound:
// once no further match can be reported, later files are not opened at all.
func TestSearchStopsReadingOnceTheBudgetIsSpent(t *testing.T) {
	content := strings.Repeat(strings.Repeat("x", 200)+"alpha\n", 5)
	source := worktree(t, "repo", map[string]string{
		"a.txt": content, "b.txt": content, "c.txt": content, "d.txt": content,
	}, nil)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alpha"}, MaxResponseBytes: 600})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.FilesSearched != 1 {
		t.Fatalf("searched %d files; the budget was spent inside the first", result.FilesSearched)
	}
	if !result.Truncated {
		t.Fatal("expected Truncated")
	}
}

func TestSearchReportsExhaustiveResponseBytes(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": "alpha\nbeta\nalpha\n"}, nil)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"alpha"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.Truncated || len(result.Notes) != 0 {
		t.Fatalf("an exhaustive response must not report truncation: %+v", result)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(result.Matches))
	}
	var want int64
	for _, match := range result.Matches {
		want += matchBytes(match)
	}
	if result.ResponseBytes != want {
		t.Fatalf("reported %d bytes, want %d", result.ResponseBytes, want)
	}
}

func TestSearchResponseBudgetAccountsContextLines(t *testing.T) {
	source := worktree(t, "repo", map[string]string{
		"a.txt": strings.Repeat(strings.Repeat("y", 100)+"\n", 3) + "alpha\n" +
			strings.Repeat(strings.Repeat("z", 100)+"\n", 3),
	}, nil)
	service := NewService([]Source{source})
	bare, err := service.Search(context.Background(), Request{Patterns: []string{"alpha"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	withContext, err := service.Search(context.Background(),
		Request{Patterns: []string{"alpha"}, ContextLines: 3})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Three leading and three trailing context lines of 100 bytes each.
	if withContext.ResponseBytes != bare.ResponseBytes+6*(100+lineFramingBytes) {
		t.Fatalf("context lines must count against the budget: %d then %d",
			bare.ResponseBytes, withContext.ResponseBytes)
	}
	// The same match with context no longer fits a budget the bare match did.
	clipped, err := service.Search(context.Background(), Request{Patterns: []string{"alpha"},
		ContextLines: 3, MaxResponseBytes: bare.ResponseBytes + 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(clipped.Matches) != 1 {
		t.Fatalf("the only match must still be reported, got %d", len(clipped.Matches))
	}
	if clipped.ResponseBytes <= bare.ResponseBytes+1 {
		t.Fatalf("the single oversized match must be reported whole, got %d bytes", clipped.ResponseBytes)
	}
}

// TestSearchReportsEveryBoundThatClipped covers the budget interacting with
// the count caps: both clip the same response and both are named.
func TestSearchReportsEveryBoundThatClipped(t *testing.T) {
	line := strings.Repeat("x", 300) + " hit hit\n"
	source := worktree(t, "repo", map[string]string{
		"a.txt": strings.Repeat(line, 20),
		"b.txt": strings.Repeat(line, 20),
	}, nil)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"hit"}, MaxMatchesPerFile: 4, MaxResponseBytes: 1800})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.ResponseBytes > 1800 {
		t.Fatalf("response accounted %d bytes, over the 1800 budget", result.ResponseBytes)
	}
	if !result.Truncated {
		t.Fatal("expected Truncated")
	}
	if !hasNote(result.Notes, "max_response_bytes budget of 1800 reached; results are truncated") {
		t.Fatalf("notes %v missing the payload-budget note", result.Notes)
	}
	if !hasNote(result.Notes, "max_matches_per_file cap of 4 clipped matches in 1 file(s): repo/a.txt") {
		t.Fatalf("notes %v missing the per-file note", result.Notes)
	}
	if !hasNote(result.Notes, truncationDisclaimer) {
		t.Fatalf("notes %v missing the truncation disclaimer", result.Notes)
	}
}

// TestSearchCountCapsGovernUnderAGenerousBudget proves the budget does not
// quietly take over from the documented count caps.
func TestSearchCountCapsGovernUnderAGenerousBudget(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": strings.Repeat("hit\n", 400)}, nil)
	result, err := NewService([]Source{source}).Search(context.Background(),
		Request{Patterns: []string{"hit"}, MaxMatches: 120, MaxMatchesPattern: 120,
			MaxMatchesPerFile: 50, MaxResponseBytes: MaxResponseBytesCeiling})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 50 {
		t.Fatalf("got %d matches, want the per-file cap of 50", len(result.Matches))
	}
	if hasNote(result.Notes, "max_response_bytes budget of 262144 reached; results are truncated") {
		t.Fatalf("the budget must not clip here: %v", result.Notes)
	}
}

func TestSearchRejectsBoundsAboveTheirCeilings(t *testing.T) {
	source := worktree(t, "repo", map[string]string{"a.txt": "alpha\n"}, nil)
	service := NewService([]Source{source})
	tests := []struct {
		name    string
		request Request
		wantErr string
	}{
		{name: "total above ceiling", request: Request{Patterns: []string{"alpha"}, MaxMatches: MaxMatchesCeiling + 1},
			wantErr: "max matches must be between 1 and 500"},
		{name: "per pattern above ceiling", request: Request{Patterns: []string{"alpha"}, MaxMatchesPattern: MaxMatchesPatternCeiling + 1},
			wantErr: "max matches per pattern must be between 1 and 200"},
		{name: "per file above ceiling", request: Request{Patterns: []string{"alpha"}, MaxMatchesPerFile: MaxMatchesPerFileCeiling + 1},
			wantErr: "max matches per file must be between 1 and 50"},
		{name: "response bytes above ceiling", request: Request{Patterns: []string{"alpha"}, MaxResponseBytes: MaxResponseBytesCeiling + 1},
			wantErr: "max response bytes must be between 1 and 262144"},
		{name: "negative response bytes", request: Request{Patterns: []string{"alpha"}, MaxResponseBytes: -1},
			wantErr: "max response bytes must be between 1 and 262144"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Search(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %v does not contain %q", err, test.wantErr)
			}
		})
	}
}
