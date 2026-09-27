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
func (*Parser) Language() string { return "godot" }

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
// alias table. Both are read once per file and never guessed: a missing project
// or an unreadable table degrades to repository-relative resolution with no
// cross-file UID verification, which is reported as a diagnostic rather than
// silently changing how references resolve.
type scope struct {
	project godotid.Project
	aliases *godotid.Aliases
}

// resolve canonicalizes a Godot resource reference seen from this file, against
// the directory of the project that owns it.
func (s scope) resolve(reference string) string { return s.project.Resolve(reference) }

func newScope(input parserapi.Input) (scope, error) {
	result := scope{}
	project, projectErr := godotid.LoadProject(input.Root, input.Path)
	if strings.EqualFold(filepath.Base(input.Path), godotid.ProjectFileName) {
		// This file is the project declaration, so its own path is authoritative
		// and holds even when no filesystem is available.
		project.Path = filepath.ToSlash(input.Path)
	}
	result.project = project
	aliases, aliasErr := godotid.AliasesFor(input.Root)
	result.aliases = aliases
	if projectErr != nil {
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
