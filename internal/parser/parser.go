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
	Root    string
	Path    string
	Content []byte
	// SourcePaths is the authoritative repository-relative source membership
	// for this indexing run. A nil slice means the caller has no membership
	// snapshot and repository-wide parsers must use their deterministic fallback.
	SourcePaths []string
	Repository  string
	RepoID      string
	GoModule    string
	SemanticKey string
	// ScopeKey is the scope-local component of this file's semantic key, when
	// the caller has already derived it. A parser that would otherwise compute
	// the same value again consumes this instead. Empty means the caller has no
	// value to offer and the parser derives its own, so a missing key is never
	// read as a match.
	ScopeKey string
}

type Parser interface {
	Language() string
	Supports(path string) bool
	Parse(context.Context, Input) (graph.ParseResult, error)
}

// SemanticKeyer lets a parser add repository-level configuration to a file's
// incremental cache key. Parsers that do not depend on such configuration do
// not need to implement it.
//
// A parser that also implements WorkspaceSemanticKeyer refines that shared key
// here with the scope-local facts that change how this one file extracts.
// Callers pass the already computed workspace key as Input.SemanticKey so the
// repository-wide part is never recomputed per file.
type SemanticKeyer interface {
	SemanticKey(context.Context, Input) (string, error)
}

// ScopeKeyer reports the scope-local component of a file's semantic key on its
// own. Deriving that component can cost filesystem work, and SemanticKey folds
// it into a larger string the caller cannot take apart, so a caller that needs
// the component itself asks for it here and hands it back through
// Input.ScopeKey rather than making the parser derive it twice.
type ScopeKeyer interface {
	ScopeKey(context.Context, Input) (string, error)
}

// WorkspaceSemanticKeyer marks a semantic key that is shared by every source
// file in one indexing run, allowing the indexer to compute it once.
type WorkspaceSemanticKeyer interface {
	WorkspaceSemanticKey(context.Context, Input) (string, error)
}

// WorkspaceSemanticEvidenceKeyer provides cheap evidence for semantic inputs
// outside the repository snapshot. When every workspace keyer implements it,
// callers may reuse a previously computed workspace key while Git proves all
// in-repository inputs unchanged and these evidence keys still match.
type WorkspaceSemanticEvidenceKeyer interface {
	WorkspaceSemanticEvidenceKey(context.Context, Input) (string, error)
}

// SemanticDependencyProvider identifies repository files whose changes can
// alter this parser's output for otherwise unchanged source files.
type SemanticDependencyProvider interface {
	SemanticDependencies() []string
}

// SemanticInputProvider recognizes repository files that contribute to a
// parser's semantic model without necessarily being parsed as graph sources.
// It covers dynamic dependency names that cannot be expressed as a fixed list.
type SemanticInputProvider interface {
	IsSemanticInput(string) bool
}

// SemanticChangeProvider expands incremental invalidation when a source or
// configuration edit can change otherwise untouched parser output.
// Root is the repository root the paths are relative to, so a provider that
// keeps a per-repository model can look up the right one rather than guess.
type SemanticChangeProvider interface {
	SemanticAffectedPaths(root string, allPaths, changedPaths []string) []string
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

func (r *Registry) SemanticAffectedPaths(root string, paths, changed []string) []string {
	seen := map[string]bool{}
	for _, path := range changed {
		seen[path] = true
	}
	for _, languageParser := range r.parsers {
		provider, ok := languageParser.(SemanticChangeProvider)
		if !ok {
			continue
		}
		for _, path := range provider.SemanticAffectedPaths(root, paths, changed) {
			seen[path] = true
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func (r *Registry) WorkspaceSemanticKeys(ctx context.Context, input Input) (map[string]string, error) {
	result := map[string]string{}
	for _, languageParser := range r.parsers {
		keyer, ok := languageParser.(WorkspaceSemanticKeyer)
		if !ok {
			continue
		}
		key, err := keyer.WorkspaceSemanticKey(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("%s workspace semantic key: %w", languageParser.Language(), err)
		}
		result[languageParser.Language()] = key
	}
	return result, nil
}

// WorkspaceSemanticEvidenceKeys returns cheap external/configuration evidence
// plus whether every workspace semantic key supports safe reuse.
func (r *Registry) WorkspaceSemanticEvidenceKeys(ctx context.Context, input Input) (map[string]string, bool, error) {
	result := map[string]string{}
	cacheable := true
	for _, languageParser := range r.parsers {
		if _, ok := languageParser.(WorkspaceSemanticKeyer); !ok {
			continue
		}
		keyer, ok := languageParser.(WorkspaceSemanticEvidenceKeyer)
		if !ok {
			cacheable = false
			continue
		}
		key, err := keyer.WorkspaceSemanticEvidenceKey(ctx, input)
		if err != nil {
			return nil, false, fmt.Errorf("%s workspace semantic evidence: %w", languageParser.Language(), err)
		}
		result[languageParser.Language()] = key
	}
	return result, cacheable, nil
}

// IsSemanticDependency reports whether path is an explicit repository input
// to any parser's semantic model, even when the parser does not parse that
// file as a graph source itself.
func (r *Registry) IsSemanticDependency(path string) bool {
	path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
	base := filepath.Base(path)
	for _, languageParser := range r.parsers {
		if provider, ok := languageParser.(SemanticInputProvider); ok && provider.IsSemanticInput(path) {
			return true
		}
		provider, ok := languageParser.(SemanticDependencyProvider)
		if !ok {
			continue
		}
		for _, dependency := range provider.SemanticDependencies() {
			dependency = filepath.ToSlash(strings.TrimPrefix(dependency, "./"))
			if path == dependency || base == dependency {
				return true
			}
		}
	}
	return false
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
	b.addFact("id:"+from, graph.Fact{FromID: from}, kind, targetID, target, targetKind, loc, properties)
}

// AddNamedSourceFact records an edge whose source declaration is owned by
// another parse result. Storage resolves the exact source name and optional
// kind; the parser supplies evidence but never invents the foreign node ID.
func (b *Builder) AddNamedSourceFact(source string, sourceKind graph.NodeKind, kind graph.EdgeKind, targetID, target string, targetKind graph.NodeKind, loc graph.Location, properties map[string]string) {
	b.addFact("name:"+source, graph.Fact{Source: source, SourceKind: sourceKind}, kind,
		targetID, target, targetKind, loc, properties)
}

func (b *Builder) addFact(sourceIdentity string, source graph.Fact, kind graph.EdgeKind, targetID, target string, targetKind graph.NodeKind, loc graph.Location, properties map[string]string) {
	b.ordinal++
	keyTarget := target
	if targetID != "" {
		keyTarget = targetID
	}
	id := graph.FactID(b.Input.Path, sourceIdentity, kind, keyTarget, loc.Line, b.ordinal)
	if b.seenFact[id] {
		return
	}
	b.seenFact[id] = true
	b.Result.Facts = append(b.Result.Facts, graph.Fact{
		ID: id, FromID: source.FromID, Source: source.Source, SourceKind: source.SourceKind,
		Kind: kind, Producer: b.Language, TargetID: targetID, Target: target,
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
