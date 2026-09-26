package godot

import (
	"path/filepath"
	"strings"

	"github.com/cafecito-games/gdparser/uidfile"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

func extractUID(input parserapi.Input, file *uidfile.File) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-uid")
	if file == nil || file.UID == nil {
		return b.Finish()
	}
	loc := uidLocation(input.Path, file.UID)
	resourcePath := strings.TrimSuffix(input.Path, filepath.Ext(input.Path))
	id := b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindConfigKey, Name: file.UID.Value, QualifiedName: file.UID.Value,
		Location: loc, Properties: map[string]string{"format": "uid", "resource": resourcePath},
	})
	if target := resourceModule(resourcePath); target != "" {
		b.AddFact(id, graph.EdgeReferences, "", target, graph.KindModule, loc,
			map[string]string{"resource": resourcePath})
	}
	return b.Finish()
}

func uidLocation(path string, node uidfile.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}
