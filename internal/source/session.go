package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
)

var ErrBudget = errors.New("source budget exhausted")

type SourceBudget struct {
	MaxBytes, MaxLines int
	// SmallFileContext optionally includes a complete small file around the
	// declaration, only when both the request and excerpt line limits permit it.
	SmallFileContext int
}
type SourceUsage struct{ BytesRead, LinesReturned int }
type cachedSource struct {
	lines    []string
	complete bool
}

// Session is a request-local, non-concurrent reader. Cached bytes count once;
// every returned line counts, including repeated excerpts. It reads only regular
// files, checking cancellation between bounded chunks. An incomplete final line
// is never returned as evidence.
type Session struct {
	service *Service
	budget  SourceBudget
	usage   SourceUsage
	cache   map[string]cachedSource
}

func (s *Service) NewSession(budget SourceBudget) *Session {
	return &Session{service: s, budget: budget, cache: make(map[string]cachedSource)}
}

func (s *Session) Usage() SourceUsage { return s.usage }

func (s *Service) projectForNode(ctx context.Context, id, repository string) (indexer.Project, error) {
	if s.projects != nil {
		var matches []indexer.Project
		for _, p := range s.projects {
			if repository == "" || repository == p.Name || repository == p.ID {
				matches = append(matches, p)
			}
		}
		if len(matches) != 1 {
			return indexer.Project{}, fmt.Errorf("source repository %q resolves to %d projects", repository, len(matches))
		}
		return matches[0], nil
	}
	if s.locator == nil {
		return indexer.Project{}, errors.New("source project locator unavailable")
	}
	p, err := s.locator.ProjectForNode(ctx, id)
	if err != nil {
		return p, err
	}
	if repository != "" && repository != p.Name && repository != p.ID {
		return indexer.Project{}, errors.New("source repository does not match node repository")
	}
	return p, nil
}

// ReadNode reads a declaration already resolved by the caller. Repository is
// the owner returned with that declaration, not a user-supplied substitution.
func (s *Session) ReadNode(ctx context.Context, node graph.Node, repository string, contextLines, maxLines int) (Excerpt, error) {
	if err := ctx.Err(); err != nil {
		return Excerpt{}, err
	}
	if contextLines < 0 || contextLines > 20 || maxLines < 1 || maxLines > 1000 {
		return Excerpt{}, errors.New("invalid source line bounds")
	}
	if node.External || node.Location.Path == "" || node.Location.Line < 1 {
		return Excerpt{}, errors.New("node has no local source location")
	}
	if node.Properties["generated_binding"] == "true" {
		return Excerpt{}, errors.New("generated binding location identifies generator configuration, not implementation source; follow generated_from evidence to the schema")
	}
	remainingLines := s.budget.MaxLines - s.usage.LinesReturned
	if remainingLines <= 0 {
		return Excerpt{}, ErrBudget
	}
	p, err := s.service.projectForNode(ctx, node.ID, repository)
	if err != nil {
		return Excerpt{}, err
	}
	path, err := SafePath(p.Root, node.Location.Path)
	if err != nil {
		return Excerpt{}, err
	}
	cached, ok := s.cache[path]
	if !ok {
		remaining := s.budget.MaxBytes - s.usage.BytesRead
		if remaining <= 0 {
			return Excerpt{}, ErrBudget
		}
		// Reject named pipes before opening: opening a pipe for reading can block
		// indefinitely even though no source bytes have been consumed.
		before, err := os.Stat(path)
		if err != nil {
			return Excerpt{}, err
		}
		if !before.Mode().IsRegular() {
			return Excerpt{}, errors.New("source is not a regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return Excerpt{}, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return Excerpt{}, err
		}
		if !info.Mode().IsRegular() {
			return Excerpt{}, errors.New("source is not a regular file")
		}
		var content strings.Builder
		buffer := make([]byte, min(32768, remaining))
		complete := false
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return Excerpt{}, err
			}
			n, readErr := file.Read(buffer[:min(len(buffer), remaining)])
			s.usage.BytesRead += n
			remaining -= n
			content.Write(buffer[:n])
			if readErr == io.EOF {
				complete = true
				break
			}
			if readErr != nil {
				return Excerpt{}, readErr
			}
			if n == 0 {
				return Excerpt{}, io.ErrNoProgress
			}
		}
		// Stat proves whether a budget-sized read reached the known file end;
		// file mutation during a request is not an index freshness guarantee.
		if int64(content.Len()) >= info.Size() {
			complete = true
		}
		data := content.String()
		lines := strings.Split(data, "\n")
		if !complete || strings.HasSuffix(data, "\n") {
			lines = lines[:len(lines)-1]
		}
		if data == "" {
			lines = nil
		}
		cached = cachedSource{lines: lines, complete: complete}
		s.cache[path] = cached
	}
	if err := ctx.Err(); err != nil {
		return Excerpt{}, err
	}
	if node.Location.Line > len(cached.lines) {
		if !cached.complete {
			return Excerpt{}, ErrBudget
		}
		return Excerpt{}, errors.New("source location is beyond the current file")
	}
	symbolEnd := max(node.Location.Line, node.Location.EndLine)
	start := max(1, node.Location.Line-contextLines)
	desiredEnd := symbolEnd + contextLines
	if cached.complete && s.budget.SmallFileContext > 0 && len(cached.lines) <= min(s.budget.SmallFileContext, maxLines, remainingLines) {
		start, desiredEnd = 1, len(cached.lines)
	}
	end := min(len(cached.lines), desiredEnd, start+min(maxLines, remainingLines)-1)
	s.usage.LinesReturned += end - start + 1
	return Excerpt{Node: node, Repository: p.Name, Branch: p.Branch, Path: node.Location.Path,
		StartLine: start, EndLine: end, SymbolStart: node.Location.Line, SymbolEnd: symbolEnd,
		Content: strings.Join(cached.lines[start-1:end], "\n"), Truncated: end < symbolEnd || end < min(desiredEnd, len(cached.lines)) || !cached.complete}, nil
}
