// Package godot extracts semantic graph symbols from Godot source formats
// other than GDScript.
package godot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/gdparser/configfile"
	"github.com/cafecito-games/gdparser/shader"
	"github.com/cafecito-games/gdparser/textresource"
	"github.com/cafecito-games/gdparser/uidfile"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

// Parser handles Godot text scenes/resources, configuration, UID sidecars, and
// shaders.
type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return graph.ProducerGodot }

// WorkspaceSemanticKey fingerprints the repository-wide Godot facts that change
// how an otherwise untouched Godot file extracts: where every project.godot
// lives, which decides what a res:// reference resolves to, and which resource
// declares each uid:// alias, which decides whether a reference's UID and path
// agree. It is computed once per indexing run, and the scan it performs is the
// same one the extractors then read from cache.
func (*Parser) WorkspaceSemanticKey(_ context.Context, input parserapi.Input) (string, error) {
	aliases, err := godotid.LoadAliases(input.Root)
	if err != nil {
		return "", err
	}
	return "godot-workspace-v1:" + aliases.Digest, nil
}

func (*Parser) WorkspaceSemanticEvidenceKey(context.Context, parserapi.Input) (string, error) {
	return "godot-worktree-v1", nil
}

func (p *Parser) IsSemanticInput(path string) bool { return p.Supports(path) }

func (*Parser) Supports(path string) bool {
	if isConfigFile(path) {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tscn", ".tres", ".escn", ".gdshader", ".gdshaderinc", ".uid":
		return true
	default:
		return false
	}
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return parserapi.NewBuilder(input, "godot").Finish(), err
	}
	// Godot resource references are relative to their own project, so every
	// extractor needs the project that owns this file before it can canonicalize
	// anything. A repository without a project file yields an empty project and
	// repository-relative resolution.
	fileScope, scopeErr := newScope(input)
	var result graph.ParseResult
	var err error
	switch {
	case isConfigFile(input.Path):
		file, parseErr := configfile.ParseFile(input.Path, input.Content)
		err = parseErr
		if err == nil {
			result = extractConfig(input, fileScope, file)
		}
	case isTextResource(input.Path):
		var document *textresource.Document
		document, err = textresource.ParseFile(input.Path, input.Content)
		if err == nil {
			result = extractTextResource(input, fileScope, document)
		}
	case strings.EqualFold(filepath.Ext(input.Path), ".uid"):
		file, parseErr := uidfile.ParseFile(input.Path, input.Content)
		err = parseErr
		if err == nil {
			result = extractUID(input, file)
		}
	default:
		file, parseErr := shader.ParseFile(input.Path, input.Content)
		err = parseErr
		if err == nil {
			result = extractShader(input, fileScope, file)
		}
	}
	if err != nil {
		return parserapi.NewBuilder(input, "godot").Finish(), fmt.Errorf("parse Godot source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scopeErr != nil {
		result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{
			Path: input.Path, Line: 1, Level: "warning",
			Message: "read Godot project layout: " + scopeErr.Error(),
		})
	}
	return result, nil
}

// scope carries the Godot project that owns a file plus the repository-wide UID
// alias table. Neither is ever guessed. A repository with no project.godot at all
// resolves references repository-relatively, which is the only reading available
// and matches a single-project repository. But a project or alias table that
// could not be *read* is unknown rather than absent, and unknown must not take
// the permissive branch: resolution is refused for that file and the failure is
// reported, because resolving res:// without knowing which project owns it is a
// guess and treating an unreadable alias table as "nothing is declared" would
// approve contradictory evidence.
type scope struct {
	project godotid.Project
	aliases *godotid.Aliases
	// known is false when the owning project could not be determined, which
	// makes every reference in this file unresolvable rather than resolved
	// against an assumed project root.
	known bool
}

// resolve canonicalizes a Godot resource reference seen from this file, against
// the directory of the project that owns it.
func (s scope) resolve(reference string) string {
	if !s.known {
		return ""
	}
	return s.project.Resolve(reference)
}

// nodeGroup returns the identity a node-group name has inside the Godot project
// that owns this file, or "" when that project could not be read. An unknown
// project cannot scope a name, and a group identity that is not project-scoped
// would merge the groups of every project in a monorepo, so the reference is
// refused rather than scoped to a guess.
func (s scope) nodeGroup(name string) string {
	if !s.known || strings.TrimSpace(name) == "" {
		return ""
	}
	return s.project.NodeGroupQualifiedName(name)
}

// escapes reports whether a reference carries path evidence that leaves this
// file's Godot project, which resolves to nothing and is worth a diagnostic
// rather than silence.
func (s scope) escapes(reference string) bool {
	return godotid.EscapesProject(s.project.Dir(), reference)
}

func newScope(input parserapi.Input) (scope, error) {
	result := scope{known: true}
	project, projectErr := godotid.LoadProject(input.Root, input.Path)
	if strings.EqualFold(filepath.Base(input.Path), godotid.ProjectFileName) {
		// This file is the project declaration, so its own path is authoritative
		// and holds even when no filesystem is available.
		project.Path = filepath.ToSlash(input.Path)
	}
	result.project = project
	aliases, aliasErr := godotid.AliasesFor(input.Root)
	if aliasErr != nil {
		// An unreadable table knows nothing, so absence of a declaration proves
		// nothing either: mark it incomplete so every UID check fails closed.
		aliases = godotid.UnknownAliases(input.Root)
	}
	result.aliases = aliases
	if projectErr != nil {
		result.known = false
		return result, projectErr
	}
	return result, aliasErr
}

func isConfigFile(path string) bool {
	if strings.EqualFold(filepath.Base(path), godotid.ProjectFileName) {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cfg", ".gdextension", ".import", ".remap":
		return true
	default:
		return false
	}
}

func isTextResource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tscn", ".tres", ".escn":
		return true
	default:
		return false
	}
}

func moduleName(path string) string { return parserapi.ModuleName(path) }

func moduleLocation(path string) graph.Location {
	return graph.Location{Path: path, Line: 1, Column: 1, EndLine: 1}
}

func qualify(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}
