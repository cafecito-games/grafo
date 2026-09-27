package agentinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	linuxHome  = "/home/u"
	grafoPath  = "/opt/bin/grafo"
	cursorFile = linuxHome + "/.cursor/mcp.json"
	geminiFile = linuxHome + "/.gemini/settings.json"
	vsCodeFile = linuxHome + "/.config/Code/User/mcp.json"
	// receiptLedger is Grafo's own installed-artifact ledger, written alongside any
	// real mutation.
	receiptLedger = linuxHome + "/.config/grafo/installed-artifacts.json"
)

// configWrites returns the writes that are not Grafo's own receipt ledger.
func configWrites(environment *fakeEnvironment) []writeRecord {
	kept := make([]writeRecord, 0, len(environment.writes))
	for _, write := range environment.writes {
		if write.path != receiptLedger {
			kept = append(kept, write)
		}
	}
	return kept
}

// parseWritten parses the single configuration file the run rewrote.
func parseWritten(t *testing.T, environment *fakeEnvironment, path string) *jsonObject {
	t.Helper()
	writes := configWrites(environment)
	if len(writes) != 1 {
		t.Fatalf("configuration writes = %v, want exactly one", writes)
	}
	if writes[0].path != path {
		t.Fatalf("wrote %q, want %q", writes[0].path, path)
	}
	document, err := decodeJSONObject([]byte(writes[0].data))
	if err != nil {
		t.Fatalf("parse written configuration: %v", err)
	}
	return document
}

func child(t *testing.T, document *jsonObject, key string) *jsonObject {
	t.Helper()
	raw, ok := document.get(key)
	if !ok {
		t.Fatalf("missing key %q", key)
	}
	nested, err := decodeJSONObject(raw)
	if err != nil {
		t.Fatalf("key %q: %v", key, err)
	}
	return nested
}

func compactOf(t *testing.T, document *jsonObject, key string) string {
	t.Helper()
	raw, ok := document.get(key)
	if !ok {
		t.Fatalf("missing key %q", key)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestFileAdapterInstallPreservesUnrelatedSettings(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, cursorFile, "cursor_with_other_servers.json")
	before := environment.files[cursorFile]

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{
		Client: Client{Name: "cursor", Display: "Cursor", Scope: "user", Method: methodConfig},
		Kind:   KindMCP,
		Scope:  "user", Target: cursorFile, Change: changeInstalled,
	}}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %#v, want %#v", actions, want)
	}

	document := parseWritten(t, environment, cursorFile)
	if got := document.keys; !slices.Equal(got, []string{"mcpServers", "unrelatedTopLevel", "anotherKey"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	servers := child(t, document, "mcpServers")
	if got := servers.keys; !slices.Equal(got, []string{"github", "postgres", "grafo"}) {
		t.Fatalf("server keys = %v", got)
	}

	original, err := decodeJSONObject([]byte(before))
	if err != nil {
		t.Fatal(err)
	}
	originalServers := child(t, original, "mcpServers")
	for _, key := range []string{"github", "postgres"} {
		if got, want := compactOf(t, servers, key), compactOf(t, originalServers, key); got != want {
			t.Errorf("server %q = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"unrelatedTopLevel", "anotherKey"} {
		if got, want := compactOf(t, document, key), compactOf(t, original, key); got != want {
			t.Errorf("key %q = %s, want %s", key, got, want)
		}
	}
	if got, want := compactOf(t, servers, "grafo"), `{"command":"/opt/bin/grafo","args":["mcp"]}`; got != want {
		t.Errorf("grafo entry = %s, want %s", got, want)
	}
}

func TestFileAdapterInstallCreatesMissingConfiguration(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome)
	// The client is present (its directory exists) but has no configuration yet.
	environment.dirs[linuxHome+"/.cursor"] = true

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != changeInstalled {
		t.Fatalf("actions = %#v", actions)
	}
	if !slices.Contains(environment.mkdirs, linuxHome+"/.cursor") {
		t.Fatalf("mkdirs = %v", environment.mkdirs)
	}
	want := "{\n  \"mcpServers\": {\n    \"grafo\": {\n      \"command\": \"/opt/bin/grafo\",\n      \"args\": [\n        \"mcp\"\n      ]\n    }\n  }\n}\n"
	if got := environment.files[cursorFile]; got != want {
		t.Fatalf("configuration = %q, want %q", got, want)
	}
}

func TestFileAdapterInstallUpdatesAndDetectsUnchanged(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, cursorFile, "cursor_with_grafo.json")

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != changeUpdated {
		t.Fatalf("actions = %#v", actions)
	}
	servers := child(t, parseWritten(t, environment, cursorFile), "mcpServers")
	// The unrelated "disabled" flag on Grafo's own entry survives the update.
	if got, want := compactOf(t, servers, "grafo"), `{"command":"/opt/bin/grafo","args":["mcp"],"disabled":false}`; got != want {
		t.Fatalf("grafo entry = %s, want %s", got, want)
	}

	environment.writes = nil
	actions, err = Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != changeUnchanged {
		t.Fatalf("second run actions = %#v", actions)
	}
	if got := configWrites(environment); len(got) != 0 {
		t.Fatalf("second run wrote %v", got)
	}
}

func TestVSCodeAdapterUsesServersKeyAndTransportType(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, vsCodeFile, "vscode_with_inputs.json")

	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"vscode"}}); err != nil {
		t.Fatal(err)
	}
	document := parseWritten(t, environment, vsCodeFile)
	if got := document.keys; !slices.Equal(got, []string{"inputs", "servers"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	servers := child(t, document, "servers")
	if got := servers.keys; !slices.Equal(got, []string{"fetch", "grafo"}) {
		t.Fatalf("server keys = %v", got)
	}
	if got, want := compactOf(t, servers, "grafo"), `{"type":"stdio","command":"/opt/bin/grafo","args":["mcp"]}`; got != want {
		t.Fatalf("grafo entry = %s, want %s", got, want)
	}
}

func TestFileAdapterToleratesComments(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, geminiFile, "gemini_with_comments.json")

	if _, err := Install(context.Background(), environment, grafoPath,
		Options{Targets: []string{"gemini"}, MCPOnly: true}); err != nil {
		t.Fatal(err)
	}
	document := parseWritten(t, environment, geminiFile)
	if got := document.keys; !slices.Equal(got, []string{"theme", "mcpServers"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	if got, want := compactOf(t, document, "theme"), `"Default"`; got != want {
		t.Fatalf("theme = %s, want %s", got, want)
	}
	servers := child(t, document, "mcpServers")
	if got := servers.keys; !slices.Equal(got, []string{"memory", "grafo"}) {
		t.Fatalf("server keys = %v", got)
	}
}

func TestFileAdapterUninstallRemovesOnlyGrafo(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, cursorFile, "cursor_with_grafo.json")
	original, err := decodeJSONObject([]byte(environment.files[cursorFile]))
	if err != nil {
		t.Fatal(err)
	}

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != changeRemoved {
		t.Fatalf("actions = %#v", actions)
	}
	servers := child(t, parseWritten(t, environment, cursorFile), "mcpServers")
	if got := servers.keys; !slices.Equal(got, []string{"github"}) {
		t.Fatalf("server keys = %v", got)
	}
	originalServers := child(t, original, "mcpServers")
	if got, want := compactOf(t, servers, "github"), compactOf(t, originalServers, "github"); got != want {
		t.Fatalf("github entry = %s, want %s", got, want)
	}

	environment.writes = nil
	actions, err = Uninstall(context.Background(), environment, Options{Targets: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != changeUnchanged {
		t.Fatalf("second uninstall actions = %#v", actions)
	}
	if got := configWrites(environment); len(got) != 0 {
		t.Fatalf("second uninstall wrote %v", got)
	}
}

func TestFileAdapterUninstallRefusesForeignServer(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome).withFixture(t, cursorFile, "cursor_conflicting_grafo.json")
	before := environment.files[cursorFile]

	_, err := Uninstall(context.Background(), environment, Options{Targets: []string{"cursor"}})
	if err == nil || !strings.Contains(err.Error(), "is not grafo") {
		t.Fatalf("error = %v", err)
	}
	if environment.files[cursorFile] != before || len(environment.writes) != 0 {
		t.Fatal("configuration was modified")
	}
}

func TestFileAdapterRefusesMalformedConfiguration(t *testing.T) {
	for _, operation := range []string{"install", "uninstall"} {
		t.Run(operation, func(t *testing.T) {
			environment := newFakeEnvironment("linux", linuxHome).withFixture(t, cursorFile, "malformed.json")
			before := environment.files[cursorFile]

			var err error
			if operation == "install" {
				_, err = Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}})
			} else {
				_, err = Uninstall(context.Background(), environment, Options{Targets: []string{"cursor"}})
			}
			if err == nil || !strings.Contains(err.Error(), "is malformed") {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(err.Error(), cursorFile) || !strings.Contains(err.Error(), "Cursor") {
				t.Fatalf("error does not name the client and path: %v", err)
			}
			if environment.files[cursorFile] != before || len(environment.writes) != 0 {
				t.Fatal("configuration was modified")
			}
		})
	}
}

func TestFileAdapterPreservesIndentation(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome)
	environment.files[cursorFile] = "{\n\t\"mcpServers\": {}\n}\n"

	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"cursor"}}); err != nil {
		t.Fatal(err)
	}
	if got := environment.files[cursorFile]; !strings.Contains(got, "\n\t\"mcpServers\"") {
		t.Fatalf("configuration = %q", got)
	}
}

func TestOwnedByGrafo(t *testing.T) {
	cases := []struct {
		command   string
		arguments []string
		want      bool
	}{
		{command: "/opt/bin/grafo", arguments: []string{"mcp"}, want: true},
		{command: `C:\tools\grafo.exe`, arguments: []string{"mcp"}, want: true},
		{command: "/opt/bin/grafo", arguments: []string{"serve"}},
		{command: "/opt/bin/grafo", arguments: nil},
		{command: "/usr/bin/other", arguments: []string{"mcp"}},
	}
	for _, test := range cases {
		if got := ownedByGrafo(test.command, test.arguments); got != test.want {
			t.Errorf("ownedByGrafo(%q, %v) = %t, want %t", test.command, test.arguments, got, test.want)
		}
	}
}
