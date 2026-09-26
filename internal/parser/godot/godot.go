// Package godot extracts semantic graph symbols from Godot source formats
// other than GDScript.
package godot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/gdparser/projectconfig"
	"github.com/cafecito-games/gdparser/shader"
	"github.com/cafecito-games/gdparser/textresource"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

// Parser handles Godot text scenes/resources, project settings, and shaders.
type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "godot" }

func (*Parser) Supports(path string) bool {
	if strings.EqualFold(filepath.Base(path), "project.godot") {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tscn", ".tres", ".escn", ".gdshader", ".gdshaderinc":
		return true
	default:
		return false
	}
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return parserapi.NewBuilder(input, "godot").Finish(), err
	}
	var result graph.ParseResult
	var err error
	switch {
	case strings.EqualFold(filepath.Base(input.Path), "project.godot"):
		file, parseErr := projectconfig.ParseFile(input.Path, input.Content)
		err = parseErr
		if err == nil {
			result = extractProject(input, file)
		}
	case isTextResource(input.Path):
		var document *textresource.Document
		document, err = textresource.ParseFile(input.Path, input.Content)
		if err == nil {
			result = extractTextResource(input, document)
		}
	default:
		file, parseErr := shader.ParseFile(input.Path, input.Content)
		err = parseErr
		if err == nil {
			result = extractShader(input, file)
		}
	}
	if err != nil {
		return parserapi.NewBuilder(input, "godot").Finish(), fmt.Errorf("parse Godot source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
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

func resourceModule(path string) string {
	path = strings.TrimSpace(strings.TrimPrefix(path, "*"))
	path = strings.TrimPrefix(path, "res://")
	path = strings.TrimPrefix(path, "user://")
	return parserapi.ModuleName(filepath.ToSlash(path))
}

func moduleLocation(path string) graph.Location {
	return graph.Location{Path: path, Line: 1, Column: 1, EndLine: 1}
}

func qualify(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}
