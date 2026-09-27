package agentinstall

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
)

// Client is one supported MCP client.
type Client struct {
	Name    string `json:"name"`    // stable lowercase identity, e.g. "claude"
	Display string `json:"display"` // e.g. "Claude Code"
	Scope   string `json:"scope"`   // e.g. "user"
	Method  string `json:"method"`  // "cli" or "config"
}

const (
	methodCLI    = "cli"
	methodConfig = "config"
)

// registry is the single source of truth for supported clients, their scopes,
// their configuration locations, and their command shapes. Clients are listed
// in a deterministic order (by name) that every caller observes.
var registry = []adapter{
	// Claude Code exposes `claude mcp add/remove/list --scope user`.
	cliAdapter{
		identity:   Client{Name: "claude", Display: "Claude Code", Scope: "user", Method: methodCLI},
		executable: "claude",
		addArguments: func(_ context.Context, _ Reader, _, grafoPath string) []string {
			return []string{"mcp", "add", "--scope", "user", serverName, "--", grafoPath, "mcp"}
		},
		removeArguments: []string{"mcp", "remove", "--scope", "user", serverName},
		listArguments:   []string{"mcp", "list"},
		retryOnExists:   true,
	},
	// Claude Desktop reads claude_desktop_config.json from its user data
	// directory: ~/Library/Application Support/Claude on macOS,
	// %APPDATA%\Claude on Windows, ~/.config/Claude elsewhere.
	fileAdapter{
		identity:     Client{Name: "claude-desktop", Display: "Claude Desktop", Scope: "user", Method: methodConfig},
		containerKey: "mcpServers",
		configPath: func(reader Reader) (string, error) {
			return appDataPath(reader, "Claude", "claude_desktop_config.json")
		},
	},
	// Cline stores MCP servers in its VS Code extension settings directory.
	fileAdapter{
		identity:     Client{Name: "cline", Display: "Cline", Scope: "user", Method: methodConfig},
		containerKey: "mcpServers",
		configPath: func(reader Reader) (string, error) {
			return vsCodeUserPath(reader, "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
		},
	},
	// Codex exposes `codex mcp add/remove/list`.
	cliAdapter{
		identity:   Client{Name: "codex", Display: "Codex", Scope: "user", Method: methodCLI},
		executable: "codex",
		addArguments: func(_ context.Context, _ Reader, _, grafoPath string) []string {
			return []string{"mcp", "add", serverName, "--", grafoPath, "mcp"}
		},
		removeArguments: []string{"mcp", "remove", serverName},
		listArguments:   []string{"mcp", "list"},
	},
	// Cursor reads global MCP servers from ~/.cursor/mcp.json on every platform.
	fileAdapter{
		identity:     Client{Name: "cursor", Display: "Cursor", Scope: "user", Method: methodConfig},
		containerKey: "mcpServers",
		configPath: func(reader Reader) (string, error) {
			return homePath(reader, ".cursor", "mcp.json")
		},
	},
	// Gemini CLI reads user-level MCP servers from ~/.gemini/settings.json.
	fileAdapter{
		identity:     Client{Name: "gemini", Display: "Gemini CLI", Scope: "user", Method: methodConfig},
		containerKey: "mcpServers",
		configPath: func(reader Reader) (string, error) {
			return homePath(reader, ".gemini", "settings.json")
		},
	},
	// OpenCode exposes `opencode mcp add/remove`; older builds lack --global.
	cliAdapter{
		identity:   Client{Name: "opencode", Display: "OpenCode", Scope: "user", Method: methodCLI},
		executable: "opencode",
		addArguments: func(ctx context.Context, reader Reader, executablePath, grafoPath string) []string {
			arguments := []string{"mcp", "add", serverName}
			help, _ := reader.Output(ctx, executablePath, "mcp", "add", "--help")
			if bytes.Contains(help, []byte("--global")) {
				arguments = append(arguments, "--global")
			}
			return append(arguments, "--", grafoPath, "mcp")
		},
		removeArguments: []string{"mcp", "remove", serverName},
	},
	// VS Code reads user-level MCP servers from mcp.json in the user profile
	// directory, keyed under "servers" with an explicit transport type.
	fileAdapter{
		identity:             Client{Name: "vscode", Display: "VS Code", Scope: "user", Method: methodConfig},
		containerKey:         "servers",
		includeTransportType: true,
		configPath: func(reader Reader) (string, error) {
			return vsCodeUserPath(reader, "mcp.json")
		},
	},
	// Windsurf reads ~/.codeium/windsurf/mcp_config.json on every platform.
	fileAdapter{
		identity:     Client{Name: "windsurf", Display: "Windsurf", Scope: "user", Method: methodConfig},
		containerKey: "mcpServers",
		configPath: func(reader Reader) (string, error) {
			return homePath(reader, ".codeium", "windsurf", "mcp_config.json")
		},
	},
}

// aliases maps historical and colloquial target words onto client names.
var aliases = map[string]string{
	"claude-code":        "claude",
	"claudecode":         "claude",
	"claude-desktop":     "claude-desktop",
	"claudedesktop":      "claude-desktop",
	"code":               "vscode",
	"gemini-cli":         "gemini",
	"vs-code":            "vscode",
	"visual-studio-code": "vscode",
}

// Clients returns every supported client in deterministic order.
func Clients() []Client {
	clients := make([]Client, 0, len(registry))
	for _, entry := range registry {
		clients = append(clients, entry.client())
	}
	return clients
}

func clientNames() []string {
	names := make([]string, 0, len(registry))
	for _, entry := range registry {
		names = append(names, entry.client().Name)
	}
	return names
}

func normalizeTarget(target string) string {
	name := strings.ToLower(strings.TrimSpace(target))
	if replacement, ok := aliases[name]; ok {
		return replacement
	}
	return name
}

// selectAdapters resolves target words to adapters. It reports whether the
// selection is automatic, in which case missing clients are skipped rather than
// treated as errors.
func selectAdapters(targets []string, all bool) ([]adapter, bool, error) {
	if all || len(targets) == 0 {
		return slices.Clone(registry), true, nil
	}

	names := make([]string, 0, len(targets))
	for _, target := range targets {
		name := normalizeTarget(target)
		if name == "" {
			continue
		}
		if name == "all" {
			if len(targets) != 1 {
				return nil, false, fmt.Errorf("client target %q cannot be combined with other targets", target)
			}
			return slices.Clone(registry), true, nil
		}
		if slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return slices.Clone(registry), true, nil
	}

	selected := make([]adapter, 0, len(names))
	for _, name := range names {
		index := slices.IndexFunc(registry, func(candidate adapter) bool { return candidate.client().Name == name })
		if index == -1 {
			return nil, false, fmt.Errorf("unsupported client %q (supported: %s, all)", name, strings.Join(clientNames(), ", "))
		}
		selected = append(selected, registry[index])
	}
	return selected, false, nil
}
