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
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/pathscope"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
)

// Documented defaults applied when the matching Request field is zero. They
// are deliberately small: search answers content questions the graph does not
// model, so a default response is meant to be evidence an agent can read, not
// an exhaustive index dump. A caller that needs more raises the matching field
// explicitly, up to the ceilings below.
const (
	// DefaultMaxMatchesPerFile bounds matches reported for a single file.
	DefaultMaxMatchesPerFile = 10
	// DefaultMaxMatchesPattern bounds matches reported for a single pattern.
	DefaultMaxMatchesPattern = 50
	// DefaultMaxMatches bounds matches reported for the whole request.
	DefaultMaxMatches = 100
	// DefaultMaxFileBytes bounds the size of a file that will be searched.
	DefaultMaxFileBytes int64 = 1 << 20
	// DefaultMaxResponseBytes bounds the match payload one response carries,
	// independently of how many matches satisfy the count caps.
	DefaultMaxResponseBytes int64 = 32 << 10
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
	// MaxMatchesPerFileCeiling is the largest MaxMatchesPerFile a caller may
	// request.
	MaxMatchesPerFileCeiling = 50
	// MaxMatchesPatternCeiling is the largest MaxMatchesPattern a caller may
	// request.
	MaxMatchesPatternCeiling = 200
	// MaxMatchesCeiling is the largest MaxMatches a caller may request.
	MaxMatchesCeiling = 500
	// MaxResponseBytesCeiling is the largest MaxResponseBytes a caller may
	// request.
	MaxResponseBytesCeiling int64 = 256 << 10

	// binarySniffBytes is the leading window examined for a NUL byte.
	binarySniffBytes = 8000
	// maxLineBytes bounds a single scanned line.
	maxLineBytes = 1 << 20
	// cancellationInterval is how often ctx is rechecked inside one file.
	cancellationInterval = 512
	// skipExampleLimit bounds how many paths a skip note names.
	skipExampleLimit = 3
	// matchFramingBytes is charged per reported match on top of its text, and
	// lineFramingBytes per context line, so the response budget accounts for
	// the envelope each carries rather than only the source text. The budget
	// bounds that accounted payload; a serialized response is a roughly
	// proportional multiple of it, since indentation and escaping depend on
	// the encoder and on the content.
	matchFramingBytes int64 = 64
	lineFramingBytes  int64 = 4
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
	MaxResponseBytes  int64 // total reported match payload cap, in bytes
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
	ResponseBytes int64    `json:"response_bytes"`  // accounted payload of Matches
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
	perFile       int
	perPattern    int
	total         int
	fileBytes     int64
	responseBytes int64
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
	clippedBytes  bool
	reportedBytes int64
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
	prefixes, err := normalizePrefixes(request.PathPrefixes)
	if err != nil {
		return Result{}, err
	}
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
			if state.exhausted() {
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
		if state.exhausted() {
			break
		}
	}
	return state.result(), nil
}

// exhausted reports whether a bound has stopped the search, so no later file
// can contribute a reported match.
func (c *collector) exhausted() bool {
	return c.clippedTotal || c.clippedBytes
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
	matches, reported := c.fit(matches)
	result := Result{Matches: matches, FilesSearched: c.filesSearched,
		FilesSkipped: c.filesSkipped, ResponseBytes: reported}
	var notes []string
	if c.clippedBytes {
		notes = append(notes, fmt.Sprintf("max_response_bytes budget of %d reached; results are truncated",
			c.bounds.responseBytes))
	}
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
	result.Truncated = c.clippedTotal || c.clippedBytes ||
		len(c.clippedFiles) > 0 || len(c.clippedPatts) > 0
	if result.Truncated {
		notes = append(notes, truncationDisclaimer)
	}
	if len(notes) > 0 {
		result.Notes = notes
	}
	return result
}

// truncationDisclaimer keeps a clipped response from reading as an exhaustive
// one. A truncated search reports part of the evidence, so it can never
// support a claim that nothing else matches.
const truncationDisclaimer = "truncated results are a partial view; a pattern or path absent from this " +
	"response is not evidence that it does not occur elsewhere"

// fit clips the reported matches to the response budget and returns them with
// the bytes they account for. Matches are already in reporting order, so the
// retained set is the deterministic prefix of that order. The first match is
// always kept, so a single oversized match is reported rather than turning the
// response into an empty, truncated one.
func (c *collector) fit(matches []Match) ([]Match, int64) {
	var reported int64
	for index := range matches {
		cost := matchBytes(matches[index])
		if index > 0 && reported+cost > c.bounds.responseBytes {
			c.clippedBytes = true
			return matches[:index], reported
		}
		reported += cost
	}
	return matches, reported
}

// matchBytes is what one reported match costs against the response budget: the
// bytes of every string it carries, plus the envelope allowances.
func matchBytes(match Match) int64 {
	total := int64(len(match.Pattern) + len(match.Repository) + len(match.Branch) +
		len(match.Path) + len(match.Language) + len(match.Text))
	for _, line := range match.Before {
		total += int64(len(line)) + lineFramingBytes
	}
	for _, line := range match.After {
		total += int64(len(line)) + lineFramingBytes
	}
	return total + matchFramingBytes
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
				c.reportedBytes += int64(len(line)) + lineFramingBytes
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
					// Stop collecting once the budget is spent. Trailing
					// context for matches already recorded still completes, so
					// fit applies the exact bound to the reported set.
					cost := matchBytes(match)
					if c.total > 0 && c.reportedBytes+cost > c.bounds.responseBytes {
						c.clippedBytes = true
						capped = true
						break
					}
					c.reportedBytes += cost
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
		total: request.MaxMatches, fileBytes: request.MaxFileBytes,
		responseBytes: request.MaxResponseBytes}
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
	if limits.responseBytes == 0 {
		limits.responseBytes = DefaultMaxResponseBytes
	}
	if limits.perFile < 1 || limits.perFile > MaxMatchesPerFileCeiling {
		return bounds{}, fmt.Errorf("max matches per file must be between 1 and %d, got %d",
			MaxMatchesPerFileCeiling, request.MaxMatchesPerFile)
	}
	if limits.perPattern < 1 || limits.perPattern > MaxMatchesPatternCeiling {
		return bounds{}, fmt.Errorf("max matches per pattern must be between 1 and %d, got %d",
			MaxMatchesPatternCeiling, request.MaxMatchesPattern)
	}
	if limits.total < 1 || limits.total > MaxMatchesCeiling {
		return bounds{}, fmt.Errorf("max matches must be between 1 and %d, got %d",
			MaxMatchesCeiling, request.MaxMatches)
	}
	if limits.fileBytes < 1 || limits.fileBytes > MaxFileBytesCeiling {
		return bounds{}, fmt.Errorf("max file bytes must be between 1 and %d, got %d", MaxFileBytesCeiling, request.MaxFileBytes)
	}
	if limits.responseBytes < 1 || limits.responseBytes > MaxResponseBytesCeiling {
		return bounds{}, fmt.Errorf("max response bytes must be between 1 and %d, got %d",
			MaxResponseBytesCeiling, request.MaxResponseBytes)
	}
	return limits, nil
}

// normalizePrefixes validates, deduplicates, and orders repository-relative
// segment prefixes. Invalid prefixes are rejected here, so search refuses the
// same prefixes every other surface refuses instead of silently matching
// nothing against a value that can never name a repository path.
func normalizePrefixes(prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	for _, prefix := range prefixes {
		normalized, err := pathscope.NormalizePrefix(prefix)
		if err != nil {
			return nil, fmt.Errorf("invalid path prefix %q: %w", prefix, err)
		}
		set[normalized] = true
	}
	result := make([]string, 0, len(set))
	for prefix := range set {
		result = append(result, prefix)
	}
	sort.Strings(result)
	return result, nil
}

// matchesPrefix reports whether path lies under one of the prefixes. Matching
// respects whole path segments, so prefix "client" selects "client/main.gd"
// but never "client-old/main.gd".
func matchesPrefix(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if pathscope.HasPrefix(path, prefix) {
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
