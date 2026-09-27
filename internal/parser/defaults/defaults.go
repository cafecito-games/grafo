// Package defaults owns the production parser registry shared by the CLI and
// deterministic evaluation corpus.
package defaults

import (
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	javaparser "github.com/cafecito-games/grafo/internal/parser/java"
	manifestparser "github.com/cafecito-games/grafo/internal/parser/manifest"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	protobufparser "github.com/cafecito-games/grafo/internal/parser/protobuf"
	"github.com/cafecito-games/grafo/internal/parser/protobufbinding"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	sqliteparser "github.com/cafecito-games/grafo/internal/parser/sql/sqlite"
	swiftparser "github.com/cafecito-games/grafo/internal/parser/swift"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
)

func NewRegistry() *parserapi.Registry {
	bindings := protobufbinding.NewLoader()
	return parserapi.NewRegistry(
		protobufbinding.New(bindings), gdscriptparser.NewWithBindingLoader(bindings), godotparser.New(), golangparser.NewWithBindingLoader(bindings), javaparser.New(), pythonparser.New(), protobufparser.New(), swiftparser.New(), typescriptparser.New(),
		manifestparser.New(), markdownparser.New(), sqlparser.New(postgresparser.New(), sqliteparser.New()), configparser.New(),
	)
}
