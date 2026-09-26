// Package agentinstall registers Grafo's MCP server with supported coding agents.
package agentinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const serverName = "grafo"

type agent struct {
	name       string
	display    string
	executable string
}

var supportedAgents = []agent{
	{name: "claude", display: "Claude Code", executable: "claude"},
	{name: "codex", display: "Codex", executable: "codex"},
	{name: "opencode", display: "OpenCode", executable: "opencode"},
}

// Runner provides the process operations needed to configure an agent.
type Runner interface {
	LookPath(file string) (string, error)
	Output(ctx context.Context, name string, arguments ...string) ([]byte, error)
	Run(ctx context.Context, name string, arguments ...string) ([]byte, error)
}

// ExecRunner configures agents through their installed command-line tools.
type ExecRunner struct{}

func (ExecRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (ExecRunner) Output(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).Output()
}

func (ExecRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
}

// Result describes one successfully configured agent.
type Result struct {
	Agent string
	Scope string
}

// Install registers executable as the Grafo MCP server. With no targets (or
// the target "all"), every supported agent found on PATH is configured.
func Install(ctx context.Context, runner Runner, executable string, targets []string) ([]Result, error) {
	if strings.TrimSpace(executable) == "" {
		return nil, fmt.Errorf("locate grafo executable: empty path")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("locate grafo executable: %w", err)
	}

	selected, automatic, err := selectAgents(targets)
	if err != nil {
		return nil, err
	}

	var results []Result
	var installErrors []error
	for _, candidate := range selected {
		agentPath, lookErr := runner.LookPath(candidate.executable)
		if lookErr != nil {
			if automatic {
				continue
			}
			installErrors = append(installErrors, fmt.Errorf("%s is not installed or is not on PATH", candidate.display))
			continue
		}

		arguments, scope := installArguments(ctx, runner, candidate, executable, agentPath)
		output, runErr := runner.Run(ctx, agentPath, arguments...)
		if candidate.name == "claude" && runErr != nil && bytes.Contains(bytes.ToLower(output), []byte("already exists")) {
			// Claude refuses to update an existing entry, while Codex and OpenCode
			// replace it. Remove only Grafo's user-scoped entry, then retry.
			removeOutput, removeErr := runner.Run(ctx, agentPath, "mcp", "remove", "--scope", "user", serverName)
			if removeErr != nil {
				installErrors = append(installErrors, commandError("replace "+candidate.display+" configuration", removeOutput, removeErr))
				continue
			}
			output, runErr = runner.Run(ctx, agentPath, arguments...)
		}
		if runErr != nil {
			installErrors = append(installErrors, commandError("configure "+candidate.display, output, runErr))
			continue
		}
		results = append(results, Result{Agent: candidate.display, Scope: scope})
	}

	if automatic && len(results) == 0 && len(installErrors) == 0 {
		return nil, fmt.Errorf("no supported agents found on PATH (supported: claude, codex, opencode)")
	}
	return results, errors.Join(installErrors...)
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("%s: %s", action, detail)
}

func selectAgents(targets []string) ([]agent, bool, error) {
	if len(targets) == 0 {
		return supportedAgents, true, nil
	}

	names := make([]string, 0, len(targets))
	for _, target := range targets {
		name := strings.ToLower(strings.TrimSpace(target))
		if name == "all" {
			if len(targets) != 1 {
				return nil, false, fmt.Errorf("agent target %q cannot be combined with other targets", target)
			}
			return supportedAgents, true, nil
		}
		if name == "claude-code" {
			name = "claude"
		}
		if slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}

	selected := make([]agent, 0, len(names))
	for _, name := range names {
		index := slices.IndexFunc(supportedAgents, func(candidate agent) bool { return candidate.name == name })
		if index == -1 {
			return nil, false, fmt.Errorf("unsupported agent %q (supported: claude, codex, opencode, all)", name)
		}
		selected = append(selected, supportedAgents[index])
	}
	return selected, false, nil
}

func installArguments(ctx context.Context, runner Runner, candidate agent, grafoPath, agentPath string) ([]string, string) {
	switch candidate.name {
	case "claude":
		return []string{"mcp", "add", "--scope", "user", serverName, "--", grafoPath, "mcp"}, "user"
	case "codex":
		return []string{"mcp", "add", serverName, "--", grafoPath, "mcp"}, "user"
	case "opencode":
		arguments := []string{"mcp", "add", serverName}
		help, _ := runner.Output(ctx, agentPath, "mcp", "add", "--help")
		if bytes.Contains(help, []byte("--global")) {
			arguments = append(arguments, "--global")
		}
		return append(arguments, "--", grafoPath, "mcp"), "user"
	default:
		panic("unsupported agent: " + candidate.name)
	}
}
