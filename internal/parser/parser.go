package parser

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

type Input struct {
	Root       string
	Path       string
	Content    []byte
	Repository string
	RepoID     string
	GoModule   string
}

type Parser interface {
	Language() string
	Supports(path string) bool
	Parse(context.Context, Input) (graph.ParseResult, error)
}

type Registry struct {
	parsers []Parser
}

func NewRegistry(parsers ...Parser) *Registry { return &Registry{parsers: parsers} }

func (r *Registry) For(path string) (Parser, bool) {
	for _, p := range r.parsers {
		if p.Supports(path) {
			return p, true
		}
	}
	return nil, false
}

func (r *Registry) Languages() []string {
	result := make([]string, 0, len(r.parsers))
	for _, p := range r.parsers {
		result = append(result, p.Language())
	}
	sort.Strings(result)
	return result
}

func FileNode(input Input, language string) graph.Node {
	return graph.Node{
		ID: graph.NodeID(graph.KindFile, input.RepoID+":"+input.Path), Kind: graph.KindFile,
		Name: filepath.Base(input.Path), QualifiedName: input.Path, Language: language,
		Location: graph.Location{Path: input.Path, Line: 1, Column: 1}, OwnerFile: input.Path,
	}
}

type Builder struct {
	Input    Input
	Language string
	Result   graph.ParseResult
	ordinal  int
	seenNode map[string]bool
	seenFact map[string]bool
}

func NewBuilder(input Input, language string) *Builder {
	b := &Builder{Input: input, Language: language, seenNode: map[string]bool{}, seenFact: map[string]bool{}}
	b.AddNode(FileNode(input, language))
	return b
}

func (b *Builder) FileID() string {
	return graph.NodeID(graph.KindFile, b.Input.RepoID+":"+b.Input.Path)
}

func (b *Builder) AddNode(node graph.Node) string {
	if node.ID == "" {
		node.ID = graph.NodeID(node.Kind, node.QualifiedName, b.Input.RepoID, node.Location.Path)
	}
	if node.OwnerFile == "" {
		node.OwnerFile = b.Input.Path
	}
	if node.Language == "" {
		node.Language = b.Language
	}
	if !b.seenNode[node.ID] {
		b.Result.Nodes = append(b.Result.Nodes, node)
		b.seenNode[node.ID] = true
	}
	return node.ID
}

func (b *Builder) AddFact(from string, kind graph.EdgeKind, targetID, target string, targetKind graph.NodeKind, loc graph.Location, properties map[string]string) {
	b.ordinal++
	keyTarget := target
	if targetID != "" {
		keyTarget = targetID
	}
	id := graph.FactID(b.Input.Path, from, kind, keyTarget, loc.Line, b.ordinal)
	if b.seenFact[id] {
		return
	}
	b.seenFact[id] = true
	b.Result.Facts = append(b.Result.Facts, graph.Fact{
		ID: id, FromID: from, Kind: kind, TargetID: targetID, Target: target,
		TargetKind: targetKind, Location: loc, Properties: properties, OwnerFile: b.Input.Path,
	})
}

func (b *Builder) Declare(parent string, node graph.Node) string {
	id := b.AddNode(node)
	b.AddFact(parent, graph.EdgeDeclares, id, "", "", node.Location, nil)
	return id
}

func (b *Builder) Diagnostic(line int, level, message string) {
	b.Result.Diagnostics = append(b.Result.Diagnostics, graph.Diagnostic{
		Path: b.Input.Path, Line: line, Level: level, Message: message,
	})
}

func (b *Builder) Finish() graph.ParseResult {
	sort.Slice(b.Result.Nodes, func(i, j int) bool { return b.Result.Nodes[i].ID < b.Result.Nodes[j].ID })
	sort.Slice(b.Result.Facts, func(i, j int) bool { return b.Result.Facts[i].ID < b.Result.Facts[j].ID })
	return b.Result
}

func ModuleName(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	path = strings.TrimSuffix(filepath.ToSlash(path), ext)
	path = strings.TrimSuffix(path, "/index")
	return strings.TrimPrefix(path, "./")
}

func Unquote(text string) string {
	text = strings.TrimSpace(text)
	if len(text) < 2 {
		return text
	}
	first, last := text[0], text[len(text)-1]
	if (first == '\'' || first == '"' || first == '`') && last == first {
		return text[1 : len(text)-1]
	}
	return text
}

func Unsupported(path string) error { return fmt.Errorf("no parser for %s", path) }
