package agentinstall

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type invocation struct {
	name      string
	arguments []string
}

type fakeRunner struct {
	paths       map[string]string
	help        map[string]string
	runErrors   map[string]error
	onRun       func(name string, arguments []string) ([]byte, error)
	invocations []invocation
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if path := f.paths[file]; path != "" {
		return path, nil
	}
	return "", errors.New("not found")
}

func (f *fakeRunner) Output(_ context.Context, name string, arguments ...string) ([]byte, error) {
	return []byte(f.help[name]), nil
}

func (f *fakeRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	f.invocations = append(f.invocations, invocation{name: name, arguments: arguments})
	if f.onRun != nil {
		return f.onRun(name, arguments)
	}
	if err := f.runErrors[name]; err != nil {
		return []byte("agent command failed"), err
	}
	return nil, nil
}

func TestInstallAutoDetectsAgents(t *testing.T) {
	runner := &fakeRunner{
		paths: map[string]string{"claude": "/bin/claude", "opencode": "/bin/opencode"},
		help:  map[string]string{"/bin/opencode": "Usage: opencode mcp add --global"},
	}

	results, err := Install(context.Background(), runner, "/opt/grafo", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantResults := []Result{{Agent: "Claude Code", Scope: "user"}, {Agent: "OpenCode", Scope: "user"}}
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("results = %#v, want %#v", results, wantResults)
	}
	wantInvocations := []invocation{
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", "/opt/grafo", "mcp"}},
		{name: "/bin/opencode", arguments: []string{"mcp", "add", "grafo", "--global", "--", "/opt/grafo", "mcp"}},
	}
	if !reflect.DeepEqual(runner.invocations, wantInvocations) {
		t.Fatalf("invocations = %#v, want %#v", runner.invocations, wantInvocations)
	}
}

func TestInstallReplacesExistingClaudeConfiguration(t *testing.T) {
	addCalls := 0
	runner := &fakeRunner{paths: map[string]string{"claude": "/bin/claude"}}
	runner.onRun = func(_ string, arguments []string) ([]byte, error) {
		if len(arguments) > 1 && arguments[1] == "add" {
			addCalls++
			if addCalls == 1 {
				return []byte("MCP server grafo already exists in user config"), errors.New("exit 1")
			}
		}
		return nil, nil
	}

	_, err := Install(context.Background(), runner, "/opt/grafo", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	want := []invocation{
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", "/opt/grafo", "mcp"}},
		{name: "/bin/claude", arguments: []string{"mcp", "remove", "--scope", "user", "grafo"}},
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", "/opt/grafo", "mcp"}},
	}
	if !reflect.DeepEqual(runner.invocations, want) {
		t.Fatalf("invocations = %#v, want %#v", runner.invocations, want)
	}
}

func TestInstallTargetsOlderOpenCode(t *testing.T) {
	runner := &fakeRunner{paths: map[string]string{"opencode": "/bin/opencode"}, help: map[string]string{}}

	_, err := Install(context.Background(), runner, "/opt/grafo", []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	want := []invocation{{name: "/bin/opencode", arguments: []string{"mcp", "add", "grafo", "--", "/opt/grafo", "mcp"}}}
	if !reflect.DeepEqual(runner.invocations, want) {
		t.Fatalf("invocations = %#v, want %#v", runner.invocations, want)
	}
}

func TestInstallReportsMissingExplicitAgent(t *testing.T) {
	runner := &fakeRunner{paths: map[string]string{}}

	_, err := Install(context.Background(), runner, "/opt/grafo", []string{"codex"})
	if err == nil || err.Error() != "Codex is not installed or is not on PATH" {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallContinuesAfterAgentFailure(t *testing.T) {
	runner := &fakeRunner{
		paths:     map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"},
		runErrors: map[string]error{"/bin/claude": errors.New("exit 1")},
	}

	results, err := Install(context.Background(), runner, "/opt/grafo", []string{"claude", "codex"})
	if err == nil || err.Error() != "configure Claude Code: agent command failed" {
		t.Fatalf("error = %v", err)
	}
	want := []Result{{Agent: "Codex", Scope: "user"}}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results = %#v, want %#v", results, want)
	}
}

func TestInstallRejectsUnknownAgent(t *testing.T) {
	_, err := Install(context.Background(), &fakeRunner{}, "/opt/grafo", []string{"cursor"})
	if err == nil || err.Error() != `unsupported agent "cursor" (supported: claude, codex, opencode, all)` {
		t.Fatalf("error = %v", err)
	}
}
