// Package search provides bounded literal and RE2 content search over files
// that belong to refreshed Grafo indexes. Content is read from the active
// worktree of each indexed project and is never persisted or logged.
package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
)

// Documented defaults applied when the matching Request field is zero.
const (
	// DefaultMaxMatchesPerFile bounds matches reported for a single file.
	DefaultMaxMatchesPerFile = 50
	// DefaultMaxMatchesPattern bounds matches reported for a single pattern.
	DefaultMaxMatchesPattern = 200
	// DefaultMaxMatches bounds matches reported for the whole request.
	DefaultMaxMatches = 500
	// DefaultMaxFileBytes bounds the size of a file that will be searched.
	DefaultMaxFileBytes int64 = 1 << 20
)

const (
	// MaxContextLines matches the bound used by internal/source.
	MaxContextLines = 20
	// MaxPatterns rejects oversized pattern batches before any work happens.
	MaxPatterns = 64
	// MaxPatternBytes bounds compiled pattern complexity. Combined with the
	// byte, file and match bounds it keeps pathological patterns cheap.
	MaxPatternBytes = 1024
	// MaxFileBytesCeiling is the largest MaxFileBytes a caller may request.
	MaxFileBytesCeiling int64 = 16 << 20

	// binarySniffBytes is the leading window examined for a NUL byte.
	binarySniffBytes = 8000
	// maxLineBytes bounds a single scanned line.
	maxLineBytes = 1 << 20
	// cancellationInterval is how often ctx is rechecked inside one file.
	cancellationInterval = 512
	// skipExampleLimit bounds how many paths a skip note names.
	skipExampleLimit = 3
)

// Source is one searchable repository index: the file catalog plus the
// worktree its records refer to.
type Source struct {
	Project indexer.Project
	Catalog graph.FileCatalog
}

// Request describes one bounded search over the configured sources.
type Request struct {
	Patterns          []string // one or more literal or RE2 patterns
	Regex             bool     // treat Patterns as RE2 instead of literals
	CaseSensitive     bool
	PathPrefixes      []string // optional repository-relative path filters
	Languages         []string // optional language filters, matched against FileRecord.Language
	Repositories      []string // optional repository name filters
	ContextLines      int
	MaxMatchesPerFile int
	MaxMatchesPattern int // per-pattern cap
	MaxMatches        int // total cap
	MaxFileBytes      int64
}

// Match is one matched span. Line and Column are 1-based, Column counting
// bytes from the start of the line, consistent with graph.Location.
type Match struct {
	Pattern    string   `json:"pattern"`
	Repository string   `json:"repository"`
	Branch     string   `json:"branch,omitempty"`
	Path       string   `json:"path"`
	Language   string   `json:"language,omitempty"`
	Line       int      `json:"line"`
	Column     int      `json:"column"`
	Text       string   `json:"text"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

// Result is the deterministic outcome of one search.
type Result struct {
	Matches       []Match  `json:"matches"`
	FilesSearched int      `json:"files_searched"`
	FilesSkipped  int      `json:"files_skipped"`
	Truncated     bool     `json:"truncated"`
	Notes         []string `json:"notes,omitempty"` // e.g. which caps clipped results
}

// Service searches the worktrees of a fixed set of indexed repositories.
type Service struct {
	sources []Source
}

// NewService returns a search service bound to the given sources. Sources are
// copied and ordered by repository name then root so results never depend on
// caller or filesystem enumeration order.
func NewService(sources []Source) *Service {
	ordered := make([]Source, 0, len(sources))
	ordered = append(ordered, sources...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Project.Name != ordered[j].Project.Name {
			return ordered[i].Project.Name < ordered[j].Project.Name
		}
		return ordered[i].Project.Root < ordered[j].Project.Root
	})
	return &Service{sources: ordered}
}

// Sources returns the repositories this service may search.
func (s *Service) Sources() []Source {
	result := make([]Source, 0, len(s.sources))
	return append(result, s.sources...)
}

type compiled struct {
	pattern string
	regex   *regexp.Regexp // nil in literal mode
	literal string         // lower-cased when the search is case-insensitive
}

type bounds struct {
	perFile    int
	perPattern int
	total      int
	fileBytes  int64
}

type skipReason int

const (
	skipBinary skipReason = iota
	skipOversized
	skipUnreadable
	skipOutsideRoot
)

var skipReasonOrder = []skipReason{skipBinary, skipOversized, skipUnreadable, skipOutsideRoot}

type collector struct {
	request       Request
	bounds        bounds
	matches       []Match
	total         int
	perPattern    map[string]int
	clippedFiles  map[string]bool
	clippedPatts  map[string]bool
	clippedTotal  bool
	filesSearched int
	filesSkipped  int
	skips         map[skipReason][]string
}

// Search runs the request against every configured source and returns
// deterministically ordered matches.
func (s *Service) Search(ctx context.Context, request Request) (Result, error) {
	if len(s.sources) == 0 {
		return Result{}, errors.New("search has no indexed repositories available")
	}
	patterns, err := compilePatterns(request)
	if err != nil {
		return Result{}, err
	}
	limits, err := resolveBounds(request)
	if err != nil {
		return Result{}, err
	}
	if request.ContextLines < 0 || request.ContextLines > MaxContextLines {
		return Result{}, fmt.Errorf("context lines must be between 0 and %d", MaxContextLines)
	}
	prefixes := normalizePrefixes(request.PathPrefixes)
	languages := lowerSet(request.Languages)
	repositories := lowerSet(request.Repositories)

	state := &collector{request: request, bounds: limits,
		perPattern: map[string]int{}, clippedFiles: map[string]bool{},
		clippedPatts: map[string]bool{}, skips: map[skipReason][]string{}}

	for _, source := range s.sources {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if len(repositories) > 0 && !repositories[strings.ToLower(source.Project.Name)] {
			continue
		}
		if source.Catalog == nil {
			return Result{}, fmt.Errorf("repository %s has no file catalog", source.Project.Name)
		}
		files, err := source.Catalog.Files(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("list indexed files for repository %s: %w", source.Project.Name, err)
		}
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if state.clippedTotal {
				break
			}
			record := files[path]
			if !matchesPrefix(path, prefixes) {
				continue
			}
			if len(languages) > 0 && !languages[strings.ToLower(record.Language)] {
				continue
			}
			if err := state.searchFile(ctx, source, record, patterns); err != nil {
				return Result{}, err
			}
		}
		if state.clippedTotal {
			break
		}
	}
	return state.result(), nil
}

func (c *collector) result() Result {
	matches := c.matches
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Pattern < b.Pattern
	})
	if matches == nil {
		matches = []Match{}
	}
	result := Result{Matches: matches, FilesSearched: c.filesSearched, FilesSkipped: c.filesSkipped}
	var notes []string
	if c.clippedTotal {
		notes = append(notes, fmt.Sprintf("max_matches cap of %d reached; results are truncated", c.bounds.total))
	}
	if len(c.clippedFiles) > 0 {
		notes = append(notes, fmt.Sprintf("max_matches_per_file cap of %d clipped matches in %d file(s): %s",
			c.bounds.perFile, len(c.clippedFiles), strings.Join(sortedKeys(c.clippedFiles), ", ")))
	}
	if len(c.clippedPatts) > 0 {
		for _, pattern := range sortedKeys(c.clippedPatts) {
			notes = append(notes, fmt.Sprintf("max_matches_pattern cap of %d clipped matches for pattern %q",
				c.bounds.perPattern, pattern))
		}
	}
	for _, reason := range skipReasonOrder {
		paths := c.skips[reason]
		if len(paths) == 0 {
			continue
		}
		sort.Strings(paths)
		examples := paths
		if len(examples) > skipExampleLimit {
			examples = examples[:skipExampleLimit]
		}
		notes = append(notes, fmt.Sprintf("%s: %d file(s) including %s", skipDescription(reason, c.bounds.fileBytes), len(paths), strings.Join(examples, ", ")))
	}
	if len(notes) > 0 {
		result.Notes = notes
	}
	result.Truncated = c.clippedTotal || len(c.clippedFiles) > 0 || len(c.clippedPatts) > 0
	return result
}

func skipDescription(reason skipReason, fileBytes int64) string {
	switch reason {
	case skipBinary:
		return "skipped binary content"
	case skipOversized:
		return fmt.Sprintf("skipped content exceeding max_file_bytes of %d", fileBytes)
	case skipOutsideRoot:
		return "skipped paths that resolve outside the repository root"
	default:
		return "skipped unreadable or missing files"
	}
}

func (c *collector) skip(reason skipReason, repository, path string) {
	c.filesSkipped++
	key := repository + "/" + path
	c.skips[reason] = append(c.skips[reason], key)
}

// searchFile streams one file and records matches. Per-file problems never
// fail the whole search; they are counted as skips instead.
func (c *collector) searchFile(ctx context.Context, source Source, record graph.FileRecord, patterns []compiled) error {
	absolute, err := sourcecontext.SafePath(source.Project.Root, record.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.skip(skipUnreadable, source.Project.Name, record.Path)
			return nil
		}
		c.skip(skipOutsideRoot, source.Project.Name, record.Path)
		return nil
	}
	info, err := os.Stat(absolute)
	if err != nil {
		c.skip(skipUnreadable, source.Project.Name, record.Path)
		return nil
	}
	if !info.Mode().IsRegular() {
		c.skip(skipUnreadable, source.Project.Name, record.Path)
		return nil
	}
	if info.Size() > c.bounds.fileBytes {
		c.skip(skipOversized, source.Project.Name, record.Path)
		return nil
	}
	file, err := os.Open(absolute)
	if err != nil {
		c.skip(skipUnreadable, source.Project.Name, record.Path)
		return nil
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReaderSize(io.LimitReader(file, c.bounds.fileBytes+1), binarySniffBytes)
	head, err := reader.Peek(binarySniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		c.skip(skipUnreadable, source.Project.Name, record.Path)
		return nil
	}
	if bytes.IndexByte(head, 0) >= 0 {
		c.skip(skipBinary, source.Project.Name, record.Path)
		return nil
	}

	fileMatches, skipped, err := c.scan(ctx, reader, source, record, patterns)
	if err != nil {
		return err
	}
	if skipped {
		c.skip(skipUnreadable, source.Project.Name, record.Path)
		return nil
	}
	c.filesSearched++
	c.matches = append(c.matches, fileMatches...)
	return nil
}

// pending is a recorded match still waiting for trailing context lines.
type pending struct {
	index int
	line  int
}

func (c *collector) scan(ctx context.Context, reader io.Reader, source Source, record graph.FileRecord, patterns []compiled) ([]Match, bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	contextLines := c.request.ContextLines
	var before []string
	var found []Match
	var waiting []pending
	perFile := 0
	lineNumber := 0
	capped := false

	for scanner.Scan() {
		lineNumber++
		if lineNumber%cancellationInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		line := scanner.Text()

		// Fill trailing context for earlier matches in this file.
		remaining := waiting[:0]
		for _, item := range waiting {
			if lineNumber-item.line <= contextLines {
				found[item.index].After = append(found[item.index].After, line)
			}
			if lineNumber-item.line < contextLines {
				remaining = append(remaining, item)
			}
		}
		waiting = remaining

		if !capped {
			for _, pattern := range patterns {
				for _, column := range pattern.find(line, c.request.CaseSensitive) {
					if c.total >= c.bounds.total {
						c.clippedTotal = true
						capped = true
						break
					}
					if perFile >= c.bounds.perFile {
						c.clippedFiles[source.Project.Name+"/"+record.Path] = true
						capped = true
						break
					}
					if c.perPattern[pattern.pattern] >= c.bounds.perPattern {
						c.clippedPatts[pattern.pattern] = true
						break
					}
					match := Match{Pattern: pattern.pattern, Repository: source.Project.Name,
						Branch: source.Project.Branch, Path: record.Path, Language: record.Language,
						Line: lineNumber, Column: column, Text: line}
					if contextLines > 0 && len(before) > 0 {
						match.Before = append(match.Before, before...)
					}
					found = append(found, match)
					if contextLines > 0 {
						waiting = append(waiting, pending{index: len(found) - 1, line: lineNumber})
					}
					perFile++
					c.total++
					c.perPattern[pattern.pattern]++
				}
				if capped {
					break
				}
			}
		}
		if capped && len(waiting) == 0 {
			break
		}
		if contextLines > 0 {
			before = append(before, line)
			if len(before) > contextLines {
				before = before[len(before)-contextLines:]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, ctx.Err()) && ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return found, false, nil
}

// find returns the 1-based byte columns of every non-overlapping match of the
// pattern within line.
func (p compiled) find(line string, caseSensitive bool) []int {
	var columns []int
	if p.regex != nil {
		for _, span := range p.regex.FindAllStringIndex(line, -1) {
			columns = append(columns, span[0]+1)
		}
		return columns
	}
	haystack := line
	if !caseSensitive {
		haystack = strings.ToLower(line)
	}
	if p.literal == "" {
		return nil
	}
	offset := 0
	for offset <= len(haystack)-len(p.literal) {
		index := strings.Index(haystack[offset:], p.literal)
		if index < 0 {
			break
		}
		columns = append(columns, offset+index+1)
		offset += index + len(p.literal)
	}
	return columns
}

func compilePatterns(request Request) ([]compiled, error) {
	if len(request.Patterns) == 0 {
		return nil, errors.New("at least one search pattern is required")
	}
	if len(request.Patterns) > MaxPatterns {
		return nil, fmt.Errorf("search accepts at most %d patterns, got %d", MaxPatterns, len(request.Patterns))
	}
	seen := map[string]bool{}
	result := make([]compiled, 0, len(request.Patterns))
	for _, pattern := range request.Patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, errors.New("search pattern must not be empty")
		}
		if len(pattern) > MaxPatternBytes {
			return nil, fmt.Errorf("search pattern %q exceeds %d bytes", truncatePattern(pattern), MaxPatternBytes)
		}
		if !utf8.ValidString(pattern) {
			return nil, fmt.Errorf("search pattern %q is not valid UTF-8", truncatePattern(pattern))
		}
		if seen[pattern] {
			continue
		}
		seen[pattern] = true
		item := compiled{pattern: pattern}
		if request.Regex {
			expression := pattern
			if !request.CaseSensitive {
				expression = "(?i)" + expression
			}
			regex, err := regexp.Compile(expression)
			if err != nil {
				return nil, fmt.Errorf("invalid regular expression %q: %w", pattern, err)
			}
			item.regex = regex
		} else {
			item.literal = pattern
			if !request.CaseSensitive {
				item.literal = strings.ToLower(pattern)
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].pattern < result[j].pattern })
	return result, nil
}

func truncatePattern(pattern string) string {
	if len(pattern) <= 64 {
		return pattern
	}
	return pattern[:64] + "..."
}

func resolveBounds(request Request) (bounds, error) {
	limits := bounds{perFile: request.MaxMatchesPerFile, perPattern: request.MaxMatchesPattern,
		total: request.MaxMatches, fileBytes: request.MaxFileBytes}
	if limits.perFile == 0 {
		limits.perFile = DefaultMaxMatchesPerFile
	}
	if limits.perPattern == 0 {
		limits.perPattern = DefaultMaxMatchesPattern
	}
	if limits.total == 0 {
		limits.total = DefaultMaxMatches
	}
	if limits.fileBytes == 0 {
		limits.fileBytes = DefaultMaxFileBytes
	}
	if limits.perFile < 1 {
		return bounds{}, fmt.Errorf("max matches per file must be positive, got %d", request.MaxMatchesPerFile)
	}
	if limits.perPattern < 1 {
		return bounds{}, fmt.Errorf("max matches per pattern must be positive, got %d", request.MaxMatchesPattern)
	}
	if limits.total < 1 {
		return bounds{}, fmt.Errorf("max matches must be positive, got %d", request.MaxMatches)
	}
	if limits.fileBytes < 1 || limits.fileBytes > MaxFileBytesCeiling {
		return bounds{}, fmt.Errorf("max file bytes must be between 1 and %d, got %d", MaxFileBytesCeiling, request.MaxFileBytes)
	}
	return limits, nil
}

func normalizePrefixes(prefixes []string) []string {
	var result []string
	for _, prefix := range prefixes {
		clean := strings.TrimSpace(filepath.ToSlash(prefix))
		clean = strings.TrimPrefix(clean, "./")
		clean = strings.TrimPrefix(clean, "/")
		clean = strings.TrimSuffix(clean, "/")
		if clean == "" || clean == "." {
			continue
		}
		result = append(result, clean)
	}
	sort.Strings(result)
	return result
}

func matchesPrefix(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	normalized := filepath.ToSlash(path)
	for _, prefix := range prefixes {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"/") || strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func lowerSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	result := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		result[strings.ToLower(trimmed)] = true
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
