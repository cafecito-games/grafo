package agentinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// serverName is the single logical name Grafo registers itself under.
const serverName = "grafo"

// serverArguments is the single normalized argument list. The command is always
// the absolute current binary and there is never any shell interpolation.
func serverArguments() []string { return []string{"mcp"} }

const (
	changeInstalled = "installed"
	changeUpdated   = "updated"
	changeRemoved   = "removed"
	changeUnchanged = "unchanged"
	changeSkipped   = "skipped"
)

// location is the read-only result of looking for a client.
type location struct {
	target     string
	found      bool
	registered bool
	detail     string
}

// fileWrite is a pending atomic configuration replacement.
type fileWrite struct {
	path string
	data []byte
	perm fs.FileMode
}

// cliRetry describes the "entry already exists" workaround for a client CLI
// that refuses to overwrite an existing server definition.
type cliRetry struct {
	marker  string
	remove  []string
	command []string
}

// plan is what an adapter intends to do. Building a plan is read-only, so
// dry-run mode can report it without touching anything.
type plan struct {
	change   string
	detail   string
	write    *fileWrite
	commands [][]string
	retry    *cliRetry
}

// adapter owns one client's identity, detection, and command or file surface.
type adapter interface {
	client() Client
	locate(ctx context.Context, reader Reader) (location, error)
	planInstall(ctx context.Context, reader Reader, place location, executable string) (plan, error)
	planUninstall(ctx context.Context, reader Reader, place location) (plan, error)
}

// apply commits a plan through the mutating half of the environment.
func (p plan) apply(ctx context.Context, env Environment, display, executable string) error {
	if p.write != nil {
		directory := parentPath(env.GOOS(), p.write.path)
		if directory != "" {
			if err := env.MkdirAll(directory, 0o755); err != nil {
				return fmt.Errorf("create %s configuration directory %s: %w", display, directory, err)
			}
		}
		if err := env.WriteFileAtomic(p.write.path, p.write.data, p.write.perm); err != nil {
			return fmt.Errorf("write %s configuration %s: %w", display, p.write.path, err)
		}
		return nil
	}
	for _, command := range p.commands {
		output, err := env.Run(ctx, executable, command...)
		if err == nil {
			continue
		}
		if p.retry == nil || !bytes.Contains(bytes.ToLower(output), []byte(p.retry.marker)) {
			return commandError("configure "+display, output, err)
		}
		// Claude Code refuses to update an existing entry and prints "already
		// exists", while Codex and OpenCode replace it. Remove only Grafo's
		// own user-scoped entry, then retry the add.
		removeOutput, removeErr := env.Run(ctx, executable, p.retry.remove...)
		if removeErr != nil {
			return commandError("replace "+display+" configuration", removeOutput, removeErr)
		}
		if output, err = env.Run(ctx, executable, p.retry.command...); err != nil {
			return commandError("configure "+display, output, err)
		}
	}
	return nil
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("%s: %s", action, detail)
}

// ---------------------------------------------------------------------------
// Client CLI adapter
// ---------------------------------------------------------------------------

// cliAdapter drives a client's own documented MCP subcommands.
type cliAdapter struct {
	identity   Client
	executable string
	// addArguments builds the add command; it may consult read-only help output.
	addArguments func(ctx context.Context, reader Reader, executablePath, grafoPath string) []string
	// removeArguments builds the remove command.
	removeArguments []string
	// listArguments is the read-only command used to detect registration.
	listArguments []string
	// retryOnExists enables the "already exists" remove-and-retry workaround.
	retryOnExists bool
}

func (a cliAdapter) client() Client { return a.identity }

func (a cliAdapter) locate(ctx context.Context, reader Reader) (location, error) {
	path, err := reader.LookPath(a.executable)
	if err != nil {
		return location{detail: a.executable + " was not found on PATH"}, nil
	}
	place := location{target: path, found: true}
	if len(a.listArguments) > 0 {
		output, listErr := reader.Output(ctx, path, a.listArguments...)
		if listErr != nil {
			place.detail = "registration state unavailable"
		} else if listNamesServer(output) {
			place.registered = true
		}
	}
	return place, nil
}

func (a cliAdapter) planInstall(ctx context.Context, reader Reader, place location, executable string) (plan, error) {
	add := a.addArguments(ctx, reader, place.target, executable)
	change := changeInstalled
	if place.registered {
		change = changeUpdated
	}
	built := plan{change: change, commands: [][]string{add}}
	if a.retryOnExists {
		built.retry = &cliRetry{marker: "already exists", remove: a.removeArguments, command: add}
	}
	return built, nil
}

func (a cliAdapter) planUninstall(_ context.Context, _ Reader, place location) (plan, error) {
	if len(a.removeArguments) == 0 {
		return plan{}, fmt.Errorf("%s does not support removing MCP servers from the command line", a.identity.Display)
	}
	if len(a.listArguments) > 0 && !place.registered && place.detail == "" {
		return plan{change: changeUnchanged, detail: "grafo is not registered"}, nil
	}
	return plan{change: changeRemoved, commands: [][]string{a.removeArguments}}, nil
}

// listNamesServer reports whether an `mcp list` style listing names grafo.
func listNamesServer(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ':' || r == '|' || r == ',' || r == '"'
		})
		for _, field := range fields {
			if field == serverName {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// File-backed adapter
// ---------------------------------------------------------------------------

// fileAdapter edits a documented user-level JSON configuration file. The file
// is parsed structurally, unknown keys and their order are preserved, and the
// replacement is written atomically.
type fileAdapter struct {
	identity Client
	// containerKey is the top-level object holding server definitions.
	containerKey string
	// includeTransportType adds "type": "stdio" to the entry, as VS Code requires.
	includeTransportType bool
	// configPath resolves the per-platform user configuration file.
	configPath func(reader Reader) (string, error)
}

func (a fileAdapter) client() Client { return a.identity }

func (a fileAdapter) locate(_ context.Context, reader Reader) (location, error) {
	path, err := a.configPath(reader)
	if err != nil {
		return location{}, fmt.Errorf("%s: %w", a.identity.Display, err)
	}
	place := location{target: path}
	if _, statErr := reader.Stat(path); statErr == nil {
		place.found = true
		data, readErr := reader.ReadFile(path)
		if readErr != nil {
			place.detail = "configuration unreadable: " + readErr.Error()
			return place, nil
		}
		document, parseErr := decodeJSONObject(data)
		if parseErr != nil {
			place.detail = "configuration is malformed: " + parseErr.Error()
			return place, nil
		}
		container, containerErr := containerObject(document, a.containerKey)
		if containerErr != nil {
			place.detail = containerErr.Error()
			return place, nil
		}
		place.registered = container.has(serverName)
		return place, nil
	}
	if directory := parentPath(reader.GOOS(), path); directory != "" {
		if _, statErr := reader.Stat(directory); statErr == nil {
			place.found = true
			place.detail = "configuration file not created yet"
		}
	}
	return place, nil
}

// load reads and parses the configuration, returning an empty document when the
// file does not exist.
func (a fileAdapter) load(reader Reader, path string) (*jsonObject, *jsonObject, string, error) {
	indent := "  "
	document := newJSONObject()
	data, err := reader.ReadFile(path)
	if err == nil {
		indent = detectIndent(data)
		document, err = decodeJSONObject(data)
		if err != nil {
			return nil, nil, "", fmt.Errorf("%s configuration %s is malformed: %w", a.identity.Display, path, err)
		}
	}
	container, err := containerObject(document, a.containerKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s configuration %s: %w", a.identity.Display, path, err)
	}
	return document, container, indent, nil
}

func containerObject(document *jsonObject, key string) (*jsonObject, error) {
	raw, ok := document.get(key)
	if !ok {
		return newJSONObject(), nil
	}
	container, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not an object: %w", key, err)
	}
	return container, nil
}

func (a fileAdapter) planInstall(_ context.Context, reader Reader, place location, executable string) (plan, error) {
	document, container, indent, err := a.load(reader, place.target)
	if err != nil {
		return plan{}, err
	}

	change := changeInstalled
	entry := newJSONObject()
	if existing, ok := container.get(serverName); ok {
		change = changeUpdated
		entry, err = decodeJSONObject(existing)
		if err != nil {
			return plan{}, fmt.Errorf("%s configuration %s: existing %q entry is not an object: %w",
				a.identity.Display, place.target, serverName, err)
		}
	}

	before, err := entry.compact()
	if err != nil {
		return plan{}, err
	}
	if a.includeTransportType {
		entry.set("type", json.RawMessage(`"stdio"`))
	}
	command, err := json.Marshal(executable)
	if err != nil {
		return plan{}, err
	}
	entry.set("command", command)
	arguments, err := json.Marshal(serverArguments())
	if err != nil {
		return plan{}, err
	}
	entry.set("args", arguments)
	after, err := entry.compact()
	if err != nil {
		return plan{}, err
	}
	if change == changeUpdated && bytes.Equal(before, after) {
		return plan{change: changeUnchanged, detail: "grafo is already registered"}, nil
	}

	container.set(serverName, after)
	rendered, err := a.rewrite(place.target, document, container, indent)
	if err != nil {
		return plan{}, err
	}
	return plan{change: change, write: rendered}, nil
}

func (a fileAdapter) planUninstall(_ context.Context, reader Reader, place location) (plan, error) {
	document, container, indent, err := a.load(reader, place.target)
	if err != nil {
		return plan{}, err
	}
	existing, ok := container.get(serverName)
	if !ok {
		return plan{change: changeUnchanged, detail: "grafo is not registered"}, nil
	}
	if err = verifyGrafoOwnership(a.identity.Display, place.target, existing); err != nil {
		return plan{}, err
	}
	container.remove(serverName)
	rendered, err := a.rewrite(place.target, document, container, indent)
	if err != nil {
		return plan{}, err
	}
	return plan{change: changeRemoved, write: rendered}, nil
}

// rewrite renders the document with the updated container, keeping every other
// key in place.
func (a fileAdapter) rewrite(path string, document, container *jsonObject, indent string) (*fileWrite, error) {
	encoded, err := container.compact()
	if err != nil {
		return nil, err
	}
	document.set(a.containerKey, encoded)
	data, err := document.render(indent)
	if err != nil {
		return nil, err
	}
	return &fileWrite{path: path, data: data, perm: 0o644}, nil
}

// verifyGrafoOwnership refuses to remove a server named grafo whose command is
// not a Grafo binary invoked as `grafo mcp`.
func verifyGrafoOwnership(display, path string, raw json.RawMessage) error {
	var entry struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("%s configuration %s: existing %q entry is not a stdio server: %w",
			display, path, serverName, err)
	}
	if !ownedByGrafo(entry.Command, entry.Args) {
		return fmt.Errorf("refusing to remove %q from %s configuration %s: command %q is not grafo",
			serverName, display, path, entry.Command)
	}
	return nil
}

func ownedByGrafo(command string, arguments []string) bool {
	base := command
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	base = strings.ToLower(strings.TrimSuffix(base, ".exe"))
	if base != serverName {
		return false
	}
	return len(arguments) > 0 && arguments[0] == "mcp"
}
