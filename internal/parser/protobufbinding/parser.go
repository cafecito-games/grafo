package protobufbinding

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type Parser struct{ loader *Loader }

func New(loader *Loader) *Parser {
	if loader == nil {
		loader = NewLoader()
	}
	return &Parser{loader: loader}
}

func (*Parser) Language() string { return "protobufbinding" }
func (*Parser) Supports(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "buf.gen.yaml" || base == "buf.gen.yml"
}
func (p *Parser) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	return p.loader.SemanticKey(ctx, input)
}
func (*Parser) SemanticAffectedPaths(allPaths, changedPaths []string) []string {
	changed := false
	for _, path := range changedPaths {
		if IsSemanticInput(path) {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	var result []string
	for _, path := range allPaths {
		base := strings.ToLower(filepath.Base(path))
		if base == "buf.gen.yaml" || base == "buf.gen.yml" {
			result = append(result, path)
		}
	}
	return result
}
func (p *Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, p.Language())
	registry, err := p.loader.Load(ctx, input)
	if err != nil {
		return b.Finish(), err
	}
	config, ok := registry.Config(input.Path)
	if !ok {
		return b.Finish(), nil
	}
	b.Result.Diagnostics = append(b.Result.Diagnostics, config.Diagnostics...)
	for _, item := range config.Projections {
		node := item.Node
		node.OwnerFile = input.Path
		id := b.AddNode(node)
		b.AddFact(id, graph.EdgeGeneratedFrom, item.CanonicalID, item.Canonical, item.CanonicalKind, node.Location, map[string]string{
			"generator": node.Properties["generator"], "generator_version": node.Properties["generator_version"], "adapter_version": node.Properties["adapter_version"],
		})
	}
	return b.Finish(), nil
}
