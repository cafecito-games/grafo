package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cafecito-games/grafo/internal/investigation"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
)

func investigationRequest(args parsedArguments) (investigation.Request, error) {
	if len(args.positionals) != 1 {
		return investigation.Request{}, fmt.Errorf("usage: grafo investigate <question> [--repo path] [--repo-name name] [--component name] [--path-prefix dir] [--language go] [--json]")
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil {
		return investigation.Request{}, err
	}
	request := investigation.Request{Query: args.positionals[0], Repository: args.values["repo-name"], Component: args.values["component"], PathPrefixes: prefixes, Languages: splitList(args.values["language"])}
	for _, option := range []struct {
		name        string
		destination *int
	}{
		{"max-candidates", &request.MaxCandidates}, {"max-source-lines", &request.MaxSourceLines}, {"max-source-bytes", &request.MaxSourceBytes}, {"max-output-bytes", &request.MaxOutputBytes}, {"max-work", &request.MaxWork}, {"timeout-ms", &request.TimeoutMS},
	} {
		value, err := nonNegativeIntOption(args, option.name, 0)
		if err != nil {
			return investigation.Request{}, err
		}
		*option.destination = value
	}
	return request, nil
}

func (a *App) investigate(ctx context.Context, args parsedArguments) error {
	request, err := investigationRequest(args)
	if err != nil {
		return err
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	report, err := investigation.New(repository, sourcecontext.NewScopedService(repository, projects)).Investigate(ctx, request)
	if err != nil {
		return err
	}
	// Compact JSON preserves the service's output budget; it is also the human
	// output so CLI and MCP expose exactly the same evidence and boundaries.
	content, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, string(content))
	return err
}
