package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/service"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// isolateConfiguration points every configuration lookup at throwaway
// directories and empties PATH, so no test can read or write the developer's
// real client configuration or probe installed clients.
func isolateConfiguration(t *testing.T) string {
	t.Helper()
	home := testtemp.Dir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("PATH", testtemp.Dir(t))
	return home
}

// runCLI runs one command and returns its exit status and combined output.
func runCLI(t *testing.T, arguments ...string) (int, string) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	status := New(stdout, stderr).Run(context.Background(), arguments)
	return status, stdout.String() + stderr.String()
}

func TestParseArgumentsAcceptsServiceAndDoctorOptions(t *testing.T) {
	parsed, err := parseArguments([]string{"service", "add", ".", "--interval", "10s", "--paused", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.command != "service" || len(parsed.positionals) != 2 || parsed.positionals[0] != "add" {
		t.Fatalf("unexpected parse: %#v", parsed)
	}
	if !parsed.flags["paused"] || parsed.values["interval"] != "10s" {
		t.Fatalf("unexpected options: %#v %#v", parsed.flags, parsed.values)
	}
	parsed, err = parseArguments([]string{"doctor", "--repair", "--json", "--state-dir", "/tmp/state"})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.flags["repair"] || parsed.values["state-dir"] != "/tmp/state" {
		t.Fatalf("unexpected doctor options: %#v %#v", parsed.flags, parsed.values)
	}
}

func TestServiceRegistryCommandsRoundTrip(t *testing.T) {
	isolateConfiguration(t)
	root := testtemp.Dir(t)
	if status, output := runCLI(t, "service", "add", root); status != 0 || !strings.Contains(output, "registered") {
		t.Fatalf("service add = %d %q", status, output)
	}
	if status, output := runCLI(t, "service", "add", root); status != 0 || !strings.Contains(output, "already registered") {
		t.Fatalf("replayed service add = %d %q", status, output)
	}
	status, output := runCLI(t, "service", "list")
	if status != 0 || !strings.Contains(output, root) {
		t.Fatalf("service list = %d %q", status, output)
	}
	if status, output := runCLI(t, "service", "remove", root); status != 0 || !strings.Contains(output, "unregistered") {
		t.Fatalf("service remove = %d %q", status, output)
	}
	if status, output := runCLI(t, "service", "list"); status != 0 || !strings.Contains(output, "no repository roots") {
		t.Fatalf("service list after remove = %d %q", status, output)
	}
	if status, output := runCLI(t, "service", "frobnicate"); status == 0 || !strings.Contains(output, "unknown service subcommand") {
		t.Fatalf("unknown subcommand = %d %q", status, output)
	}
}

func TestServiceRunOnceIndexesRegisteredRootsAndLogsThem(t *testing.T) {
	isolateConfiguration(t)
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, output := runCLI(t, "service", "add", root); status != 0 {
		t.Fatalf("service add = %d %q", status, output)
	}
	status, output := runCLI(t, "service", "run", "--once", "--json")
	if status != 0 {
		t.Fatalf("service run --once = %d %q", status, output)
	}
	var snapshot service.Snapshot
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if len(snapshot.Roots) != 1 || snapshot.Roots[0].Updated == 0 || snapshot.Roots[0].LastError != "" {
		t.Fatalf("snapshot = %#v", snapshot.Roots)
	}
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(project.IndexPath); err != nil {
		t.Fatalf("branch index missing: %v", err)
	}
	if status, output := runCLI(t, "service", "status"); status != 0 || !strings.Contains(output, root) {
		t.Fatalf("service status = %d %q", status, output)
	}
	if status, output := runCLI(t, "service", "logs"); status != 0 || !strings.Contains(output, "indexed root") {
		t.Fatalf("service logs = %d %q", status, output)
	}
}

func TestDoctorIsReadOnlyUntilRepairIsRequested(t *testing.T) {
	isolateConfiguration(t)
	root := filepath.Join(testtemp.Dir(t), "repository")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if status, output := runCLI(t, "service", "add", root); status != 0 {
		t.Fatalf("service add = %d %q", status, output)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	status, output := runCLI(t, "doctor")
	if status == 0 {
		t.Fatalf("doctor reported a missing root as healthy: %q", output)
	}
	if !strings.Contains(output, "no longer exists") {
		t.Fatalf("doctor output = %q", output)
	}
	if _, listOutput := runCLI(t, "service", "list"); !strings.Contains(listOutput, root) {
		t.Fatalf("read-only doctor changed the registry: %q", listOutput)
	}
	if status, output := runCLI(t, "doctor", "--repair"); status != 0 || !strings.Contains(output, "prune-missing-root") {
		t.Fatalf("doctor --repair = %d %q", status, output)
	}
	if _, listOutput := runCLI(t, "service", "list"); strings.Contains(listOutput, root) {
		t.Fatalf("repair did not prune the missing root: %q", listOutput)
	}
}

func TestDoctorJSONCarriesItsFormatMarker(t *testing.T) {
	isolateConfiguration(t)
	_, output := runCLI(t, "doctor", "--json")
	var diagnosis service.Diagnosis
	if err := json.Unmarshal([]byte(output), &diagnosis); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if diagnosis.Format != "grafo.doctor/1" {
		t.Fatalf("diagnosis = %#v", diagnosis)
	}
}

func TestUninstallCLIRefusesFileBackedTargetOutsideUserRoots(t *testing.T) {
	home := isolateConfiguration(t)
	outside := testtemp.Dir(t)
	if err := os.Symlink(outside, filepath.Join(home, ".cursor")); err != nil {
		t.Fatal(err)
	}

	status, output := runCLI(t, "uninstall", "cursor", "--mcp-only", "--dry-run")
	if status == 0 || !strings.Contains(output, "outside the user configuration roots") {
		t.Fatalf("uninstall = %d %q", status, output)
	}
	if _, err := os.Stat(filepath.Join(outside, "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("escaping target was created: %v", err)
	}
}

func TestHelpDocumentsServiceAndDoctor(t *testing.T) {
	_, output := runCLI(t, "help")
	for _, want := range []string{"grafo service add", "grafo service install", "grafo doctor [--repair]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help is missing %q", want)
		}
	}
}
