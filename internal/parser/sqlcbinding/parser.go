package sqlcbinding

import (
	"context"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
)

// Parser owns diagnostics on sqlc configuration files. The SQL router and this
// parser share a Loader in the production registry, so configuration is parsed
// once per immutable indexing snapshot.
type Parser struct{ loader *Loader }

func New(loader *Loader) *Parser {
	if loader == nil {
		loader = NewLoader()
	}
	return &Parser{loader: loader}
}

func (*Parser) Language() string                 { return "sqlcbinding" }
func (*Parser) Supports(path string) bool        { return IsConfigPath(path) }
func (*Parser) IsSemanticInput(path string) bool { return IsSemanticInput(path) }

func (p *Parser) WorkspaceSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	return p.loader.SemanticKey(ctx, input)
}

func (*Parser) WorkspaceSemanticEvidenceKey(context.Context, parserapi.Input) (string, error) {
	return "sqlc-binding-repository-inputs-v1", nil
}

func (p *Parser) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	workspace := input.SemanticKey
	if workspace == "" {
		var err error
		workspace, err = p.loader.SemanticKey(ctx, input)
		if err != nil {
			return "", err
		}
	}
	fileKey, err := p.loader.FileSemanticKey(ctx, input)
	if err != nil {
		return "", err
	}
	return workspace + ":" + fileKey, nil
}

func (p *Parser) SemanticAffectedPaths(root string, allPaths, changedPaths []string) []string {
	_, configs, known := p.loader.AffectedPaths(root, changedPaths)
	if known {
		return configs
	}
	for _, changed := range changedPaths {
		if !IsBindingInput(changed) {
			continue
		}
		var result []string
		for _, candidate := range allPaths {
			if IsConfigPath(candidate) {
				result = append(result, candidate)
			}
		}
		return result
	}
	return nil
}

func (p *Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	result, err := configparser.New().Parse(ctx, input)
	if err != nil {
		return result, err
	}
	registry, err := p.loader.Load(ctx, input)
	if err != nil {
		return result, err
	}
	if config, ok := registry.Config(input.Path); ok {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostics...)
	}
	return result, nil
}
