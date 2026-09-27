package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/query"
)

const maxSourceBytes = 5 << 20

type ProjectLocator interface {
	ProjectForNode(context.Context, string) (indexer.Project, error)
}

type singleProjectLocator struct {
	repository graph.QueryRepository
	project    indexer.Project
}

func NewSingleProjectLocator(repository graph.QueryRepository, project indexer.Project) ProjectLocator {
	return &singleProjectLocator{repository: repository, project: project}
}

func (l *singleProjectLocator) ProjectForNode(ctx context.Context, id string) (indexer.Project, error) {
	if _, err := l.repository.Node(ctx, id); err != nil {
		return indexer.Project{}, err
	}
	return l.project, nil
}

type Excerpt struct {
	Node        graph.Node `json:"node"`
	Repository  string     `json:"repository"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"`
	StartLine   int        `json:"start_line"`
	EndLine     int        `json:"end_line"`
	SymbolStart int        `json:"symbol_start_line"`
	SymbolEnd   int        `json:"symbol_end_line"`
	Content     string     `json:"content"`
	Truncated   bool       `json:"truncated"`
}

type Service struct {
	repository graph.QueryRepository
	locator    ProjectLocator
}

func NewService(repository graph.QueryRepository, locator ProjectLocator) *Service {
	return &Service{repository: repository, locator: locator}
}

func (s *Service) Read(ctx context.Context, selector string, contextLines, maxLines int) (Excerpt, error) {
	if strings.TrimSpace(selector) == "" {
		return Excerpt{}, errors.New("source selector is required")
	}
	if contextLines < 0 || contextLines > 20 {
		return Excerpt{}, errors.New("context lines must be between 0 and 20")
	}
	if maxLines == 0 {
		maxLines = 200
	}
	if maxLines < 1 || maxLines > 1000 {
		return Excerpt{}, errors.New("max lines must be between 1 and 1000")
	}
	node, err := query.NewService(s.repository).Resolve(ctx, selector)
	if err != nil {
		return Excerpt{}, err
	}
	if node.Location.Path == "" || node.Location.Line < 1 {
		return Excerpt{}, fmt.Errorf("node %s has no source location", node.QualifiedName)
	}
	project, err := s.locator.ProjectForNode(ctx, node.ID)
	if err != nil {
		return Excerpt{}, fmt.Errorf("locate repository for %s: %w", node.QualifiedName, err)
	}
	absolute, err := SafePath(project.Root, node.Location.Path)
	if err != nil {
		return Excerpt{}, err
	}
	content, err := readBounded(ctx, absolute)
	if err != nil {
		return Excerpt{}, fmt.Errorf("read %s: %w", node.Location.Path, err)
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if node.Location.Line > len(lines) {
		return Excerpt{}, fmt.Errorf("source location %s:%d is beyond the current file", node.Location.Path, node.Location.Line)
	}
	symbolEnd := node.Location.EndLine
	if symbolEnd < node.Location.Line {
		symbolEnd = node.Location.Line
	}
	if symbolEnd > len(lines) {
		symbolEnd = len(lines)
	}
	start := max(1, node.Location.Line-contextLines)
	desiredEnd := min(len(lines), symbolEnd+contextLines)
	end := min(desiredEnd, start+maxLines-1)
	return Excerpt{Node: node, Repository: project.Name, Branch: project.Branch,
		Path: node.Location.Path, StartLine: start, EndLine: end,
		SymbolStart: node.Location.Line, SymbolEnd: symbolEnd,
		Content: strings.Join(lines[start-1:end], "\n"), Truncated: end < desiredEnd}, nil
}

// SafePath resolves a repository-relative path against root and confines the
// result to that root: absolute paths and ".." are rejected, symlinks are
// resolved, and a path escaping the root after resolution is an error. It is
// exported so other bounded readers reuse one implementation.
func SafePath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("source path %q is absolute", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source path %q escapes the repository", relative)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	absolute := filepath.Join(resolvedRoot, clean)
	resolvedPath, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source path %q resolves outside the repository", relative)
	}
	return resolvedPath, nil
}

func readBounded(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxSourceBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxSourceBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return content, nil
}
