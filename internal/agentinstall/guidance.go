package agentinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/cafecito-games/grafo/internal/agentguide"
)

// Artifact kinds. Every Action carries one so callers can tell an MCP
// registration apart from installed guidance.
const (
	KindMCP          = "mcp"
	KindSkill        = "skill"
	KindInstructions = "instructions"
	KindHooks        = "hooks"
)

// guidanceArtifact is one Grafo-owned guidance surface for one client. Planning
// is read-only, so dry-run mode can describe every target without touching it.
type guidanceArtifact interface {
	kind() string
	// path resolves the user-scoped file this artifact owns.
	path(reader Reader) (string, error)
	// optIn reports whether the artifact is installed only on explicit request.
	optIn() bool
	planInstall(reader Reader, display, path, executable string, own ownership) (plan, error)
	planUninstall(reader Reader, display, path string, own ownership) (plan, error)
}

// clientGuidance lists the guidance surfaces of one registry client.
type clientGuidance struct {
	client    string
	artifacts []guidanceArtifact
}

// guidanceRegistry is the single source of truth for guidance surfaces. Only
// clients with a documented, user-scoped instruction surface appear here;
// the rest receive MCP registration alone. The client list itself is owned by
// registry.go and is never extended from here.
var guidanceRegistry = []clientGuidance{
	// Claude Code loads personal skills from ~/.claude/skills/<name>/SKILL.md and
	// personal hooks from ~/.claude/settings.json.
	{client: "claude", artifacts: []guidanceArtifact{
		skillFile{resolve: func(reader Reader) (string, error) {
			return homePath(reader, ".claude", "skills", agentguide.Name, "SKILL.md")
		}},
		claudeHooks{},
	}},
	// Codex reads global instructions from ~/.codex/AGENTS.md.
	{client: "codex", artifacts: []guidanceArtifact{
		instructionBlock{resolve: func(reader Reader) (string, error) {
			return homePath(reader, ".codex", "AGENTS.md")
		}},
	}},
	// Gemini CLI reads its global context file from ~/.gemini/GEMINI.md.
	{client: "gemini", artifacts: []guidanceArtifact{
		instructionBlock{resolve: func(reader Reader) (string, error) {
			return homePath(reader, ".gemini", "GEMINI.md")
		}},
	}},
	// OpenCode reads global rules from the XDG configuration directory.
	{client: "opencode", artifacts: []guidanceArtifact{
		instructionBlock{resolve: func(reader Reader) (string, error) {
			return configHomePath(reader, "opencode", "AGENTS.md")
		}},
	}},
	// Windsurf reads global rules from ~/.codeium/windsurf/memories/global_rules.md.
	{client: "windsurf", artifacts: []guidanceArtifact{
		instructionBlock{resolve: func(reader Reader) (string, error) {
			return homePath(reader, ".codeium", "windsurf", "memories", "global_rules.md")
		}},
	}},
}

// guidanceFor returns the guidance surfaces of one client in declaration order.
func guidanceFor(client string) []guidanceArtifact {
	for _, entry := range guidanceRegistry {
		if entry.client == client {
			return entry.artifacts
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shared target safety
// ---------------------------------------------------------------------------

// targetState is the read-only state of a guidance target.
type targetState struct {
	exists   bool
	contents string
	perm     fs.FileMode
}

// inspectTarget reads a guidance target and refuses anything Grafo must not
// write: a path outside the user's own configuration roots, a symlink, a
// non-regular file, or a world-writable file.
func inspectTarget(reader Reader, display, path string) (targetState, error) {
	state := targetState{perm: 0o644}
	if err := checkUserConfigRoot(reader, path); err != nil {
		return state, fmt.Errorf("%s guidance target %s: %w", display, path, err)
	}
	info, err := reader.Lstat(path)
	if err != nil {
		return state, nil
	}
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return state, fmt.Errorf("refusing to write %s guidance to %s: the path is a symlink", display, path)
	case !mode.IsRegular():
		return state, fmt.Errorf("refusing to write %s guidance to %s: the path is not a regular file", display, path)
	case mode.Perm()&0o002 != 0:
		return state, fmt.Errorf("refusing to write %s guidance to %s: mode %04o is world-writable", display, path, mode.Perm())
	}
	data, err := reader.ReadFile(path)
	if err != nil {
		return state, fmt.Errorf("read %s guidance target %s: %w", display, path, err)
	}
	state.exists = true
	state.contents = string(data)
	state.perm = mode.Perm()
	return state, nil
}

// maxSymlinkHops bounds symlink resolution so a link cycle cannot hang install.
const maxSymlinkHops = 32

// checkUserConfigRoot refuses targets that escape the user's configuration
// roots. Containment is decided on the fully resolved path, because a symlink in
// any parent directory can otherwise redirect a lexically valid target - for
// example into a repository-local instruction file, which Grafo must never write.
func checkUserConfigRoot(reader Reader, path string) error {
	separator := pathSeparator(reader.GOOS())
	for _, element := range strings.Split(path, separator) {
		if element == ".." {
			return fmt.Errorf("path escapes the user configuration root")
		}
	}
	resolved, err := resolvePath(reader, path)
	if err != nil {
		return err
	}
	for _, root := range userConfigRoots(reader) {
		if strings.TrimSpace(root) == "" {
			continue
		}
		resolvedRoot, rootErr := resolvePath(reader, root)
		if rootErr != nil {
			continue
		}
		trimmed := strings.TrimRight(resolvedRoot, "/\\")
		if resolved == trimmed || strings.HasPrefix(resolved, trimmed+separator) {
			return nil
		}
	}
	return fmt.Errorf("path %s is outside the user configuration roots", resolved)
}

// resolvePath resolves every symlinked *parent* of path and returns the real
// location. The final element is deliberately left unresolved: a symlinked
// target file is refused outright by inspectTarget rather than followed.
func resolvePath(reader Reader, path string) (string, error) {
	goos := reader.GOOS()
	separator := pathSeparator(goos)
	prefix, rest := splitPathPrefix(goos, path)
	pending := strings.Split(rest, separator)
	resolved := make([]string, 0, len(pending))
	hops := 0
	for len(pending) > 0 {
		element := pending[0]
		pending = pending[1:]
		switch element {
		case "", ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}
		candidate := prefix + strings.Join(append(slices.Clone(resolved), element), separator)
		info, err := reader.Lstat(candidate)
		// Only parents are followed; the leaf keeps its own symlink check.
		if err != nil || info.Mode()&fs.ModeSymlink == 0 || len(pending) == 0 {
			resolved = append(resolved, element)
			continue
		}
		target, linkErr := reader.Readlink(candidate)
		if linkErr != nil {
			return "", fmt.Errorf("resolve %s: %w", candidate, linkErr)
		}
		hops++
		if hops > maxSymlinkHops {
			return "", fmt.Errorf("resolve %s: too many levels of symbolic links", path)
		}
		targetPrefix, targetRest := splitPathPrefix(goos, target)
		if targetPrefix != "" {
			// An absolute link target replaces everything resolved so far.
			prefix = targetPrefix
			resolved = resolved[:0]
		}
		pending = append(strings.Split(targetRest, separator), pending...)
	}
	return prefix + strings.Join(resolved, separator), nil
}

// splitPathPrefix splits a path into its absolute prefix ("/", "C:\\", "" for a
// relative path) and the remaining elements.
func splitPathPrefix(goos, path string) (string, string) {
	separator := pathSeparator(goos)
	if goos == "windows" {
		if len(path) >= 2 && path[1] == ':' {
			return path[:2] + separator, strings.TrimLeft(path[2:], "/\\")
		}
		if strings.HasPrefix(path, separator) {
			return separator, strings.TrimLeft(path, "/\\")
		}
		return "", path
	}
	if strings.HasPrefix(path, "/") {
		return "/", strings.TrimPrefix(path, "/")
	}
	return "", path
}

// userConfigRoots lists the directories Grafo may write guidance into.
func userConfigRoots(reader Reader) []string {
	roots := make([]string, 0, 4)
	if home, err := reader.HomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots, home)
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "APPDATA"} {
		if value := strings.TrimSpace(reader.Getenv(name)); value != "" {
			roots = append(roots, value)
		}
	}
	return roots
}

// configHomePath resolves a path inside the XDG-style configuration directory:
// $XDG_CONFIG_HOME or ~/.config everywhere except Windows, where %APPDATA% is
// the documented location.
func configHomePath(reader Reader, elements ...string) (string, error) {
	if reader.GOOS() == "windows" {
		return appDataPath(reader, elements...)
	}
	if configHome := strings.TrimSpace(reader.Getenv("XDG_CONFIG_HOME")); configHome != "" {
		return joinPath(reader.GOOS(), append([]string{configHome}, elements...)...), nil
	}
	return homePath(reader, append([]string{".config"}, elements...)...)
}

// ---------------------------------------------------------------------------
// Isolated Grafo-owned skill file
// ---------------------------------------------------------------------------

// skillFile owns a whole file: its contents are generated, so a foreign file at
// the same path is a conflict rather than something to merge.
type skillFile struct {
	resolve func(Reader) (string, error)
}

func (skillFile) kind() string { return KindSkill }

func (skillFile) optIn() bool { return false }

func (s skillFile) path(reader Reader) (string, error) { return s.resolve(reader) }

func (s skillFile) planInstall(reader Reader, display, path, _ string, own ownership) (plan, error) {
	state, err := inspectTarget(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	desired := agentguide.Skill()
	digest := agentguide.Digest(desired)
	if !state.exists {
		return plan{
			change: changeInstalled,
			write:  &fileWrite{path: path, data: []byte(desired), perm: state.perm},
			digest: digest,
		}, nil
	}
	if state.contents == desired {
		return plan{change: changeUnchanged, detail: "grafo skill is current"}, nil
	}
	if !own.provesFile(path, state.contents) && !agentguide.Owns(state.contents) {
		return plan{}, fmt.Errorf("refusing to replace %s skill file %s: it carries no Grafo ownership marker",
			display, path)
	}
	return plan{
		change: changeUpdated,
		detail: "refreshed Grafo-owned skill",
		write:  &fileWrite{path: path, data: []byte(desired), perm: state.perm},
		digest: digest,
	}, nil
}

func (s skillFile) planUninstall(reader Reader, display, path string, own ownership) (plan, error) {
	state, err := inspectTarget(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	if !state.exists {
		// Nothing to delete, but a receipt for a file the user already removed
		// must not linger and claim ownership of whatever appears next.
		return plan{
			change:      changeUnchanged,
			detail:      "grafo skill is not installed",
			dropReceipt: own.provesPath(path),
		}, nil
	}
	// Deleting a whole file requires proof Grafo wrote exactly these bytes. A
	// marker substring is not proof: the user may have edited or authored the
	// file, and the issue's contract is to leave anything unprovable in place.
	if !own.provesFile(path, state.contents) {
		return plan{
			change: changeSkipped,
			detail: "grafo cannot prove it wrote this file (no matching install receipt); remove " +
				path + " manually if you no longer want it",
		}, nil
	}
	return plan{
		change:      changeRemoved,
		removes:     []string{path},
		removeDirs:  []string{parentPath(reader.GOOS(), path)},
		dropReceipt: true,
	}, nil
}

// ---------------------------------------------------------------------------
// Managed block inside a user-authored instruction file
// ---------------------------------------------------------------------------

// instructionBlock maintains exactly one delimited Grafo block inside a file the
// user also writes in. Every byte outside the markers is preserved.
type instructionBlock struct {
	resolve func(Reader) (string, error)
}

func (instructionBlock) kind() string { return KindInstructions }

func (instructionBlock) optIn() bool { return false }

func (b instructionBlock) path(reader Reader) (string, error) { return b.resolve(reader) }

func (b instructionBlock) planInstall(reader Reader, display, path, _ string, _ ownership) (plan, error) {
	state, err := inspectTarget(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	merged, change, err := agentguide.UpsertBlock(state.contents)
	if err != nil {
		return plan{}, fmt.Errorf("%s instructions %s: %w", display, path, err)
	}
	switch change {
	case agentguide.Unchanged:
		return plan{change: changeUnchanged, detail: "grafo guidance block is current"}, nil
	case agentguide.Updated:
		return plan{
			change: changeUpdated,
			detail: "refreshed the Grafo-owned block",
			write:  &fileWrite{path: path, data: []byte(merged), perm: state.perm},
			digest: agentguide.Digest(merged),
		}, nil
	default:
		return plan{
			change: changeInstalled,
			detail: "appended a delimited Grafo-owned block",
			write:  &fileWrite{path: path, data: []byte(merged), perm: state.perm},
			digest: agentguide.Digest(merged),
		}, nil
	}
}

func (b instructionBlock) planUninstall(reader Reader, display, path string, own ownership) (plan, error) {
	state, err := inspectTarget(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	if !state.exists {
		return plan{
			change:      changeUnchanged,
			detail:      "grafo guidance block is not installed",
			dropReceipt: own.provesPath(path),
		}, nil
	}
	remaining, change, err := agentguide.RemoveBlock(state.contents)
	if err != nil {
		return plan{}, fmt.Errorf("%s instructions %s: %w", display, path, err)
	}
	if change == agentguide.Unchanged {
		return plan{change: changeUnchanged, detail: "grafo guidance block is not installed"}, nil
	}
	// The file is deleted only when Grafo's own receipt claims it and nothing but
	// the managed block remained; otherwise the emptied file is left in place.
	if strings.TrimSpace(remaining) == "" && own.provesPath(path) {
		return plan{change: changeRemoved, removes: []string{path}, dropReceipt: true}, nil
	}
	return plan{
		change:      changeRemoved,
		detail:      "removed the Grafo-owned block and kept the rest of the file",
		write:       &fileWrite{path: path, data: []byte(remaining), perm: state.perm},
		dropReceipt: true,
	}, nil
}

// ---------------------------------------------------------------------------
// Advisory hooks (opt-in, documented hook API only)
// ---------------------------------------------------------------------------

// hookEvent is the Claude Code settings key advisory hooks are installed under.
const hookEvent = "PreToolUse"

// hookPhase identifies one advisory hook. The phase word is also the argument
// Grafo's own hook command receives, so ownership is provable from the command.
type hookPhase struct {
	phase   string
	matcher string
}

// hookPhases are the two advisory phases: context before a content search, and
// context before an edit. Both are advisory only: the hook command prints
// context and always exits 0, so a missing or broken Grafo cannot block a tool.
var hookPhases = []hookPhase{
	{phase: "pre-search", matcher: "Grep|Glob"},
	{phase: "pre-edit", matcher: "Edit|Write|MultiEdit|NotebookEdit"},
}

// HookCommand renders the advisory hook command line for one phase. The
// executable is shell-quoted for the target platform, because clients run hook
// commands through a shell and Grafo's absolute path may contain spaces.
func HookCommand(goos, executable, phase string) string {
	return hookCommandFor(goos, executable, phase)
}

func hookCommandFor(goos, executable, phase string) string {
	return shellQuote(goos, executable) + " guidance --hook " + phase
}

// shellSafe is the set of characters that never need quoting in a POSIX shell.
func shellSafe(character rune) bool {
	switch {
	case character >= 'a' && character <= 'z',
		character >= 'A' && character <= 'Z',
		character >= '0' && character <= '9':
		return true
	}
	return strings.ContainsRune("_@%+=:,./-", character)
}

// shellQuote quotes an executable path for the shell the client will use. Paths
// that need no quoting are emitted verbatim, so ordinary installs keep a stable
// command string across upgrades.
func shellQuote(goos, value string) string {
	if value == "" {
		return `""`
	}
	if goos == "windows" {
		// cmd.exe: wrap in double quotes and double any embedded quote.
		if !strings.ContainsAny(value, " \t\"&|<>^()") {
			return value
		}
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	safe := true
	for _, character := range value {
		if !shellSafe(character) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// claudeHooks maintains Grafo's advisory PreToolUse entries in Claude Code's
// documented personal settings file. It is opt-in because it adds a subprocess
// to the client's tool path.
type claudeHooks struct{}

func (claudeHooks) kind() string { return KindHooks }

func (claudeHooks) optIn() bool { return true }

func (claudeHooks) path(reader Reader) (string, error) {
	return homePath(reader, ".claude", "settings.json")
}

// hookEntry is the documented shape of one PreToolUse matcher entry. Unknown
// fields of user entries are never decoded: those entries stay raw.
type hookEntry struct {
	Matcher string        `json:"matcher"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

func (h claudeHooks) planInstall(reader Reader, display, path, executable string, own ownership) (plan, error) {
	document, container, indent, entries, state, err := h.load(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	commands := hookCommands(reader.GOOS(), executable)
	desired := make([]json.RawMessage, 0, len(hookPhases))
	for index, phase := range hookPhases {
		encoded, marshalErr := json.Marshal(hookEntry{
			Matcher: phase.matcher,
			Hooks:   []hookCommand{{Type: "command", Command: commands[index]}},
		})
		if marshalErr != nil {
			return plan{}, marshalErr
		}
		desired = append(desired, encoded)
	}
	// Grafo may replace only entries it can prove are its own: the exact command
	// it is about to install, or the exact command a receipt says it installed
	// before (which is how a moved binary is refreshed instead of duplicated).
	kept, owned := partitionHookEntries(entries, ownedHookPairs(commands, own.commands()))
	if len(owned) == len(desired) && sameRawMessages(owned, desired) {
		return plan{change: changeUnchanged, detail: "advisory hooks are current"}, nil
	}
	change := changeInstalled
	if len(owned) > 0 {
		change = changeUpdated
	}
	write, err := h.render(path, document, container, indent, state, append(kept, desired...))
	if err != nil {
		return plan{}, err
	}
	return plan{
		change:        change,
		detail:        "advisory " + hookEvent + " hooks (fail open; exit status is always 0)",
		write:         write,
		digest:        agentguide.Digest(string(write.data)),
		ownedCommands: commands,
	}, nil
}

func (h claudeHooks) planUninstall(reader Reader, display, path string, own ownership) (plan, error) {
	document, container, indent, entries, state, err := h.load(reader, display, path)
	if err != nil {
		return plan{}, err
	}
	if !state.exists {
		return plan{
			change:      changeUnchanged,
			detail:      "advisory hooks are not installed",
			dropReceipt: own.provesPath(path),
		}, nil
	}
	// Without receipt evidence there is no proof which entries are Grafo's, so
	// nothing is touched.
	recorded := own.commands()
	if len(recorded) == 0 {
		if !hasHookLookalike(entries) {
			return plan{change: changeUnchanged, detail: "advisory hooks are not installed"}, nil
		}
		return plan{
			change: changeSkipped,
			detail: "grafo cannot prove it installed the hooks in " + path +
				" (no matching install receipt); remove them manually if you no longer want them",
		}, nil
	}
	kept, owned := partitionHookEntries(entries, ownedHookPairs(nil, recorded))
	if len(owned) == 0 {
		return plan{
			change:      changeUnchanged,
			detail:      "advisory hooks are not installed",
			dropReceipt: own.provesPath(path),
		}, nil
	}
	write, err := h.render(path, document, container, indent, state, kept)
	if err != nil {
		return plan{}, err
	}
	return plan{change: changeRemoved, write: write, dropReceipt: true}, nil
}

// hookCommands renders the command line of every phase, in hookPhases order.
func hookCommands(goos, executable string) []string {
	commands := make([]string, 0, len(hookPhases))
	for _, phase := range hookPhases {
		commands = append(commands, hookCommandFor(goos, executable, phase.phase))
	}
	return commands
}

// ownedHookPairs lists the exact (matcher, command) pairs Grafo owns: the pairs
// it is installing now, plus the pairs a receipt proves it installed before.
func ownedHookPairs(current, recorded []string) []hookPair {
	pairs := make([]hookPair, 0, 2*len(hookPhases))
	for _, commands := range [][]string{current, recorded} {
		for index, phase := range hookPhases {
			if index < len(commands) && commands[index] != "" {
				pairs = append(pairs, hookPair{matcher: phase.matcher, command: commands[index]})
			}
		}
	}
	return pairs
}

// hookPair is one exact matcher-and-command pair Grafo may claim.
type hookPair struct {
	matcher string
	command string
}

// load parses the settings document, the hooks container, and the PreToolUse
// array. A malformed document is an error, so the file stays byte-identical.
func (h claudeHooks) load(reader Reader, display, path string) (*jsonObject, *jsonObject, string, []json.RawMessage, targetState, error) {
	state, err := inspectTarget(reader, display, path)
	if err != nil {
		return nil, nil, "", nil, state, err
	}
	indent := "  "
	document := newJSONObject()
	if state.exists {
		indent = detectIndent([]byte(state.contents))
		document, err = decodeJSONObject([]byte(state.contents))
		if err != nil {
			return nil, nil, "", nil, state, fmt.Errorf("%s settings %s are malformed: %w", display, path, err)
		}
	}
	container, err := containerObject(document, "hooks")
	if err != nil {
		return nil, nil, "", nil, state, fmt.Errorf("%s settings %s: %w", display, path, err)
	}
	var entries []json.RawMessage
	if raw, ok := container.get(hookEvent); ok {
		if err = json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, "", nil, state, fmt.Errorf("%s settings %s: %q is not an array: %w",
				display, path, hookEvent, err)
		}
	}
	return document, container, indent, entries, state, nil
}

func (h claudeHooks) render(path string, document, container *jsonObject, indent string, state targetState, entries []json.RawMessage) (*fileWrite, error) {
	if len(entries) == 0 {
		container.remove(hookEvent)
	} else {
		encoded, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		container.set(hookEvent, encoded)
	}
	encoded, err := container.compact()
	if err != nil {
		return nil, err
	}
	if string(encoded) == "{}" {
		document.remove("hooks")
	} else {
		document.set("hooks", encoded)
	}
	data, err := document.render(indent)
	if err != nil {
		return nil, err
	}
	return &fileWrite{path: path, data: data, perm: state.perm}, nil
}

// partitionHookEntries splits an existing PreToolUse array into the user's own
// entries, preserved byte-for-byte, and the entries Grafo can prove are its own.
func partitionHookEntries(entries []json.RawMessage, owned []hookPair) (kept, mine []json.RawMessage) {
	for _, entry := range entries {
		if hookEntryIsExactly(entry, owned) {
			mine = append(mine, entry)
			continue
		}
		kept = append(kept, entry)
	}
	return kept, mine
}

// hookEntryIsExactly reports whether an entry is exactly one of Grafo's own
// entries: one command hook, the exact matcher, the exact command line, and no
// extra fields anywhere that would belong to the user rather than to Grafo.
func hookEntryIsExactly(entry json.RawMessage, owned []hookPair) bool {
	fields, err := decodeJSONObject(entry)
	if err != nil {
		return false
	}
	for _, key := range fields.keys {
		if key != "matcher" && key != "hooks" {
			return false
		}
	}
	var decoded hookEntry
	if err = json.Unmarshal(entry, &decoded); err != nil || len(decoded.Hooks) != 1 {
		return false
	}
	if decoded.Hooks[0].Type != "command" {
		return false
	}
	var nested []json.RawMessage
	if err = json.Unmarshal(mustField(fields, "hooks"), &nested); err != nil {
		return false
	}
	for _, raw := range nested {
		hook, hookErr := decodeJSONObject(raw)
		if hookErr != nil {
			return false
		}
		for _, key := range hook.keys {
			if key != "type" && key != "command" {
				return false
			}
		}
	}
	for _, pair := range owned {
		if decoded.Matcher == pair.matcher && decoded.Hooks[0].Command == pair.command {
			return true
		}
	}
	return false
}

// mustField returns a decoded object's raw field, or JSON null when absent.
func mustField(object *jsonObject, key string) json.RawMessage {
	if value, ok := object.get(key); ok {
		return value
	}
	return json.RawMessage("null")
}

// hasHookLookalike reports whether any entry names Grafo's hook subcommand, so
// an unprovable cleanup can be reported instead of silently doing nothing.
func hasHookLookalike(entries []json.RawMessage) bool {
	for _, entry := range entries {
		var decoded hookEntry
		if err := json.Unmarshal(entry, &decoded); err != nil {
			continue
		}
		for _, command := range decoded.Hooks {
			if strings.Contains(command.Command, "guidance --hook ") {
				return true
			}
		}
	}
	return false
}

// sameRawMessages compares JSON values by their compact form, so re-reading an
// indented settings file does not look like a change.
func sameRawMessages(left, right []json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(compactRaw(left[index]), compactRaw(right[index])) {
			return false
		}
	}
	return true
}

func compactRaw(value json.RawMessage) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, value); err != nil {
		return value
	}
	return out.Bytes()
}
