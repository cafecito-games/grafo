package agentinstall

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// cliEnvironment returns an environment where only command-line clients exist.
func cliEnvironment(lookups map[string]string) *fakeEnvironment {
	environment := newFakeEnvironment("linux", linuxHome)
	environment.dirs[linuxHome] = true
	for name, path := range lookups {
		environment.lookups[name] = path
	}
	return environment
}

// changesByName indexes the MCP registration outcome of each client. Guidance
// artifacts are reported as their own actions and are asserted separately.
func changesByName(actions []Action) map[string]string {
	changes := make(map[string]string, len(actions))
	for _, action := range actions {
		if action.Kind == KindMCP {
			changes[action.Client.Name] = action.Change
		}
	}
	return changes
}

// mcpActions keeps only the MCP registration actions.
func mcpActions(actions []Action) []Action {
	kept := make([]Action, 0, len(actions))
	for _, action := range actions {
		if action.Kind == KindMCP {
			kept = append(kept, action)
		}
	}
	return kept
}

// actionsOfKind keeps the actions of one artifact kind for one client.
func actionsOfKind(actions []Action, client, kind string) []Action {
	kept := make([]Action, 0, len(actions))
	for _, action := range actions {
		if action.Client.Name == client && action.Kind == kind {
			kept = append(kept, action)
		}
	}
	return kept
}

func TestClientsAreDeterministicAndComplete(t *testing.T) {
	clients := Clients()
	if len(clients) != len(registry) {
		t.Fatalf("Clients() = %d entries, registry = %d", len(clients), len(registry))
	}
	names := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Name == "" || client.Display == "" || client.Scope == "" {
			t.Fatalf("incomplete client %#v", client)
		}
		if client.Method != methodCLI && client.Method != methodConfig {
			t.Fatalf("client %q has unknown method %q", client.Name, client.Method)
		}
		names = append(names, client.Name)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("clients are not in deterministic sorted order: %v", names)
	}
	want := []string{"claude", "claude-desktop", "cline", "codex", "cursor", "gemini", "opencode", "vscode", "windsurf"}
	if !slices.Equal(names, want) {
		t.Fatalf("client names = %v, want %v", names, want)
	}
	if !reflect.DeepEqual(Clients(), clients) {
		t.Fatal("Clients() is not stable across calls")
	}
}

func TestInstallAutoDetectsClients(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "opencode": "/bin/opencode"})
	environment.outputs["/bin/claude mcp list"] = "No MCP servers configured."
	environment.outputs["/bin/opencode mcp add --help"] = "Usage: opencode mcp add --global"

	actions, err := Install(context.Background(), environment, grafoPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mcpActions(actions); len(got) != len(registry) {
		t.Fatalf("MCP actions = %d, want one per client", len(got))
	}
	got := changesByName(actions)
	for name, change := range got {
		want := changeSkipped
		if name == "claude" || name == "opencode" {
			want = changeInstalled
		}
		if change != want {
			t.Errorf("%s change = %q, want %q", name, change, want)
		}
	}
	wantInvocations := []invocation{
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", grafoPath, "mcp"}},
		{name: "/bin/opencode", arguments: []string{"mcp", "add", "grafo", "--global", "--", grafoPath, "mcp"}},
	}
	if !reflect.DeepEqual(environment.invocations, wantInvocations) {
		t.Fatalf("invocations = %#v, want %#v", environment.invocations, wantInvocations)
	}
}

func TestInstallReportsNoClientsDetected(t *testing.T) {
	environment := cliEnvironment(nil)

	actions, err := Install(context.Background(), environment, grafoPath, Options{All: true})
	if err == nil || !strings.Contains(err.Error(), "no supported MCP clients detected") {
		t.Fatalf("error = %v", err)
	}
	for _, action := range actions {
		if action.Change != changeSkipped {
			t.Fatalf("action = %#v, want every client skipped", action)
		}
	}
}

func TestInstallReplacesExistingClaudeConfiguration(t *testing.T) {
	addCalls := 0
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude"})
	environment.outputs["/bin/claude mcp list"] = "grafo: /old/grafo mcp - ✓ Connected"
	environment.onRun = func(_ string, arguments []string) ([]byte, error) {
		if len(arguments) > 1 && arguments[1] == "add" {
			addCalls++
			if addCalls == 1 {
				return []byte("MCP server grafo already exists in user config"), errors.New("exit 1")
			}
		}
		return nil, nil
	}

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := mcpActions(actions); len(got) != 1 || got[0].Change != changeUpdated {
		t.Fatalf("MCP actions = %#v", got)
	}
	want := []invocation{
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", grafoPath, "mcp"}},
		{name: "/bin/claude", arguments: []string{"mcp", "remove", "--scope", "user", "grafo"}},
		{name: "/bin/claude", arguments: []string{"mcp", "add", "--scope", "user", "grafo", "--", grafoPath, "mcp"}},
	}
	if !reflect.DeepEqual(environment.invocations, want) {
		t.Fatalf("invocations = %#v, want %#v", environment.invocations, want)
	}
}

func TestInstallTargetsOlderOpenCode(t *testing.T) {
	environment := cliEnvironment(map[string]string{"opencode": "/bin/opencode"})

	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"opencode"}}); err != nil {
		t.Fatal(err)
	}
	want := []invocation{{name: "/bin/opencode", arguments: []string{"mcp", "add", "grafo", "--", grafoPath, "mcp"}}}
	if !reflect.DeepEqual(environment.invocations, want) {
		t.Fatalf("invocations = %#v, want %#v", environment.invocations, want)
	}
}

func TestInstallReportsMissingExplicitClient(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{target: "codex", want: "Codex is not installed"},
		{target: "cursor", want: "Cursor is not installed"},
		{target: "claude-desktop", want: "Claude Desktop is not installed"},
	}
	for _, test := range cases {
		t.Run(test.target, func(t *testing.T) {
			environment := cliEnvironment(nil)
			_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{test.target}})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInstallContinuesAfterClientFailure(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"})
	environment.runErrs["/bin/claude"] = errors.New("exit 1")

	actions, err := Install(context.Background(), environment, grafoPath,
		Options{Targets: []string{"claude", "codex"}, MCPOnly: true})
	if err == nil || err.Error() != "configure Claude Code: client command failed" {
		t.Fatalf("error = %v", err)
	}
	if len(actions) != 1 || actions[0].Client.Name != "codex" || actions[0].Change != changeInstalled {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestInstallRejectsUnknownTarget(t *testing.T) {
	environment := cliEnvironment(nil)
	_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"emacs"}})
	want := `unsupported client "emacs" (supported: claude, claude-desktop, cline, codex, cursor, gemini, opencode, vscode, windsurf, all)`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestTargetSelection(t *testing.T) {
	cases := []struct {
		name      string
		targets   []string
		all       bool
		want      []string
		automatic bool
		wantErr   string
	}{
		{name: "empty", want: clientNames(), automatic: true},
		{name: "all-word", targets: []string{"all"}, want: clientNames(), automatic: true},
		{name: "all-flag", targets: []string{"claude"}, all: true, want: clientNames(), automatic: true},
		{name: "legacy-claude-code", targets: []string{"claude-code"}, want: []string{"claude"}},
		{name: "legacy-trio", targets: []string{"claude", "codex", "opencode"}, want: []string{"claude", "codex", "opencode"}},
		{name: "aliases", targets: []string{"code", "vs-code", "gemini-cli"}, want: []string{"vscode", "gemini"}},
		{name: "case-and-space", targets: []string{" Cursor ", "CURSOR"}, want: []string{"cursor"}},
		{name: "order-preserved", targets: []string{"windsurf", "cline"}, want: []string{"windsurf", "cline"}},
		{name: "all-combined", targets: []string{"all", "cursor"}, wantErr: `client target "all" cannot be combined with other targets`},
		{name: "unknown", targets: []string{"nano"}, wantErr: `unsupported client "nano" (supported: claude, claude-desktop, cline, codex, cursor, gemini, opencode, vscode, windsurf, all)`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			selected, automatic, err := selectAdapters(test.targets, test.all)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if automatic != test.automatic {
				t.Fatalf("automatic = %t, want %t", automatic, test.automatic)
			}
			names := make([]string, 0, len(selected))
			for _, entry := range selected {
				names = append(names, entry.client().Name)
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("selected = %v, want %v", names, test.want)
			}
		})
	}
}

func TestInstallRefusesTemporaryExecutable(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude"})
	_, err := Install(context.Background(), environment, "/tmp/go-build123/grafo", Options{Targets: []string{"claude"}})
	if err == nil || !strings.Contains(err.Error(), "refusing to register temporary grafo executable") {
		t.Fatalf("error = %v", err)
	}
	if _, err = Install(context.Background(), environment, "   ", Options{Targets: []string{"claude"}}); err == nil {
		t.Fatal("expected an error for an empty executable path")
	}
}

func TestDetectWritesNothing(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude"}).
		withFixture(t, cursorFile, "cursor_with_grafo.json")
	environment.outputs["/bin/claude mcp list"] = "grafo: /opt/bin/grafo mcp - ✓ Connected"
	environment.strict = true

	statuses, err := Detect(context.Background(), environment, nil)
	if err != nil {
		t.Fatal(err)
	}
	environment.assertNoMutations(t)
	if len(statuses) != len(registry) {
		t.Fatalf("statuses = %d, want one per client", len(statuses))
	}
	byName := make(map[string]Status, len(statuses))
	for index, status := range statuses {
		if status.Client.Name != Clients()[index].Name {
			t.Fatalf("status order = %q at %d, want %q", status.Client.Name, index, Clients()[index].Name)
		}
		byName[status.Client.Name] = status
	}
	if got := byName["claude"]; !got.Installed || !got.Registered || got.Path != "/bin/claude" {
		t.Fatalf("claude status = %#v", got)
	}
	if got := byName["cursor"]; !got.Installed || !got.Registered || got.Path != cursorFile {
		t.Fatalf("cursor status = %#v", got)
	}
	if got := byName["codex"]; got.Installed || got.Registered {
		t.Fatalf("codex status = %#v", got)
	}
	// Even an absent file-backed client reports the path it would use.
	if got := byName["windsurf"]; got.Installed || got.Path == "" {
		t.Fatalf("windsurf status = %#v", got)
	}
}

func TestDetectRejectsUnknownTarget(t *testing.T) {
	environment := cliEnvironment(nil)
	if _, err := Detect(context.Background(), environment, []string{"atom"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	cases := []struct {
		name       string
		operation  func(*fakeEnvironment) ([]Action, error)
		wantChange map[string]string
	}{
		{
			name: "install",
			operation: func(environment *fakeEnvironment) ([]Action, error) {
				return Install(context.Background(), environment, grafoPath, Options{DryRun: true})
			},
			wantChange: map[string]string{"claude": changeUpdated, "cursor": changeUpdated, "codex": changeSkipped},
		},
		{
			name: "uninstall",
			operation: func(environment *fakeEnvironment) ([]Action, error) {
				return Uninstall(context.Background(), environment, Options{DryRun: true})
			},
			wantChange: map[string]string{"claude": changeRemoved, "cursor": changeRemoved, "codex": changeSkipped},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			environment := cliEnvironment(map[string]string{"claude": "/bin/claude"}).
				withFixture(t, cursorFile, "cursor_with_grafo.json")
			environment.outputs["/bin/claude mcp list"] = "grafo: /opt/bin/grafo mcp"
			before := environment.files[cursorFile]
			environment.strict = true

			actions, err := test.operation(environment)
			if err != nil {
				t.Fatal(err)
			}
			environment.assertNoMutations(t)
			if environment.files[cursorFile] != before {
				t.Fatal("configuration changed during a dry run")
			}
			changes := changesByName(actions)
			for name, want := range test.wantChange {
				if changes[name] != want {
					t.Errorf("%s change = %q, want %q", name, changes[name], want)
				}
			}
			for _, action := range actions {
				if !action.DryRun {
					t.Fatalf("action %#v is not marked as a dry run", action)
				}
			}
		})
	}
}

func TestDryRunMatchesRealRun(t *testing.T) {
	build := func() *fakeEnvironment {
		environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"}).
			withFixture(t, cursorFile, "cursor_with_other_servers.json")
		environment.outputs["/bin/claude mcp list"] = ""
		environment.outputs["/bin/codex mcp list"] = ""
		return environment
	}

	planned, err := Install(context.Background(), build(), grafoPath, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Install(context.Background(), build(), grafoPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != len(applied) {
		t.Fatalf("dry run produced %d actions, real run produced %d", len(planned), len(applied))
	}
	for index := range planned {
		planned[index].DryRun = false
		if !reflect.DeepEqual(planned[index], applied[index]) {
			t.Fatalf("action %d: dry run %#v, real run %#v", index, planned[index], applied[index])
		}
	}
}

func TestUninstallRemovesCommandLineRegistration(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "codex": "/bin/codex", "opencode": "/bin/opencode"})
	environment.outputs["/bin/claude mcp list"] = "grafo: /opt/bin/grafo mcp"
	environment.outputs["/bin/codex mcp list"] = "grafo  /opt/bin/grafo mcp"

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude", "codex", "opencode"}})
	if err != nil {
		t.Fatal(err)
	}
	changes := changesByName(actions)
	for _, name := range []string{"claude", "codex", "opencode"} {
		if changes[name] != changeRemoved {
			t.Fatalf("%s change = %q", name, changes[name])
		}
	}
	want := []invocation{
		{name: "/bin/claude", arguments: []string{"mcp", "remove", "--scope", "user", "grafo"}},
		{name: "/bin/codex", arguments: []string{"mcp", "remove", "grafo"}},
		{name: "/bin/opencode", arguments: []string{"mcp", "remove", "grafo"}},
	}
	if !reflect.DeepEqual(environment.invocations, want) {
		t.Fatalf("invocations = %#v, want %#v", environment.invocations, want)
	}
}

func TestUninstallSkipsAbsentClientsAutomatically(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude"})
	environment.outputs["/bin/claude mcp list"] = "grafo: /opt/bin/grafo mcp"

	actions, err := Uninstall(context.Background(), environment, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	changes := changesByName(actions)
	if changes["claude"] != changeRemoved {
		t.Fatalf("claude change = %q", changes["claude"])
	}
	for name, change := range changes {
		if name == "claude" {
			continue
		}
		if change != changeSkipped {
			t.Errorf("%s change = %q, want %q", name, change, changeSkipped)
		}
	}
}

func TestUninstallReportsUnchangedWhenNotRegistered(t *testing.T) {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude"})
	environment.outputs["/bin/claude mcp list"] = "No MCP servers configured."

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if actions = mcpActions(actions); len(actions) != 1 || actions[0].Change != changeUnchanged {
		t.Fatalf("actions = %#v", actions)
	}
	if len(environment.invocations) != 0 {
		t.Fatalf("invocations = %#v", environment.invocations)
	}
}

func TestListNamesServer(t *testing.T) {
	cases := []struct {
		output string
		want   bool
	}{
		{output: "grafo: /opt/bin/grafo mcp - ✓ Connected", want: true},
		{output: "Name   Command\ngrafo  /opt/bin/grafo mcp", want: true},
		{output: `  "grafo": {`, want: true},
		{output: "No MCP servers configured."},
		{output: "grafoextra: /bin/other"},
		{output: ""},
	}
	for _, test := range cases {
		if got := listNamesServer([]byte(test.output)); got != test.want {
			t.Errorf("listNamesServer(%q) = %t, want %t", test.output, got, test.want)
		}
	}
}

func TestServerDefinitionIsNormalized(t *testing.T) {
	if serverName != "grafo" {
		t.Fatalf("serverName = %q", serverName)
	}
	if got := serverArguments(); !slices.Equal(got, []string{"mcp"}) {
		t.Fatalf("serverArguments() = %v", got)
	}
	// The command shape never interpolates through a shell.
	for _, entry := range registry {
		cli, ok := entry.(cliAdapter)
		if !ok {
			continue
		}
		arguments := cli.addArguments(context.Background(), cliEnvironment(nil), "/bin/"+cli.executable, "/opt/bin/my grafo")
		if !slices.Contains(arguments, "/opt/bin/my grafo") {
			t.Fatalf("%s arguments = %v, want the unquoted executable", cli.identity.Name, arguments)
		}
		for _, argument := range arguments {
			if strings.ContainsAny(argument, "\"'`$") {
				t.Fatalf("%s argument %q contains shell metacharacters", cli.identity.Name, argument)
			}
		}
	}
}
