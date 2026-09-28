package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cafecito-games/grafo/internal/agentinstall"
)

// recordingEnvironment is the real filesystem with a recorded, fake process
// layer, so generated definitions and activation commands are observable for
// both launchd and systemd from one host.
type recordingEnvironment struct {
	hostEnvironment
	mutex    sync.Mutex
	commands []string
	outputs  map[string]string
	failures map[string]bool
}

func newRecordingEnvironment(t *testing.T, goos string) *recordingEnvironment {
	t.Helper()
	return &recordingEnvironment{
		hostEnvironment: isolatedHost(t, goos),
		outputs:         map[string]string{},
		failures:        map[string]bool{},
	}
}

func resolvedUserTarget(t *testing.T, env agentinstall.Environment, path string) string {
	t.Helper()
	resolved, err := agentinstall.ResolveUserConfigPath(env, path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (e *recordingEnvironment) record(name string, arguments []string) string {
	line := strings.TrimSpace(name + " " + strings.Join(arguments, " "))
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.commands = append(e.commands, line)
	return line
}

func (e *recordingEnvironment) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	line := e.record(name, arguments)
	if e.failures[line] {
		return []byte("failed"), os.ErrPermission
	}
	return []byte(e.outputs[line]), nil
}

func (e *recordingEnvironment) Output(_ context.Context, name string, arguments ...string) ([]byte, error) {
	line := e.record(name, arguments)
	output, ok := e.outputs[line]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(output), nil
}

func (e *recordingEnvironment) ran(line string) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	for _, command := range e.commands {
		if command == line {
			return true
		}
	}
	return false
}

func TestInstallGeneratesOwnedDefinitionIdempotently(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			env := newRecordingEnvironment(t, goos)
			stateDir, err := StateDir(env)
			if err != nil {
				t.Fatal(err)
			}
			binary := "/usr/local/bin/grafo"
			actions, err := Install(context.Background(), env, binary, stateDir, false)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if actions[0].Change != ChangeInstalled {
				t.Fatalf("first install = %#v", actions[0])
			}
			path := actions[0].Target
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(contents)
			if !strings.Contains(text, "grafo-service-definition: "+DefinitionVersion) {
				t.Fatalf("definition carries no version marker:\n%s", text)
			}
			if !strings.Contains(text, binary) || !strings.Contains(text, stateDir) {
				t.Fatalf("definition does not point at the absolute binary and state directory:\n%s", text)
			}
			receipt, found, err := agentinstall.OwnedFile(env, agentinstall.ServiceOwner, PlatformFor(goos).Name())
			if err != nil || !found {
				t.Fatalf("no ownership receipt was recorded: %v found=%v", err, found)
			}
			if !agentinstall.ProvesFile(env, receipt, path, text) {
				t.Fatal("receipt does not prove the definition Grafo just wrote")
			}
			replayed, err := Install(context.Background(), env, binary, stateDir, false)
			if err != nil {
				t.Fatalf("replayed install: %v", err)
			}
			if replayed[0].Change != ChangeUnchanged {
				t.Fatalf("replayed install = %#v", replayed[0])
			}
			switch goos {
			case "linux":
				if !env.ran("systemctl --user enable --now "+SystemdUnit) || !env.ran("systemctl --user daemon-reload") {
					t.Fatalf("systemd activation commands missing: %#v", env.commands)
				}
			case "darwin":
				if !env.ran("launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + path) {
					t.Fatalf("launchd activation commands missing: %#v", env.commands)
				}
			}
			state, err := Describe(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			if !state.Installed || !state.Owned || state.Conflict {
				t.Fatalf("Describe after install = %#v", state)
			}
		})
	}
}

func TestInstallReportsForeignDefinitionAndLeavesItAlone(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	path, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentinstall.ParentPath("linux", path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "[Service]\nExecStart=/usr/bin/true\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err := Install(context.Background(), env, "/usr/local/bin/grafo", stateDir, false)
	if err == nil {
		t.Fatal("expected a conflicting definition to fail closed")
	}
	if len(actions) != 1 || actions[0].Change != ChangeSkipped {
		t.Fatalf("actions = %#v", actions)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != foreign {
		t.Fatalf("foreign definition was modified: %q %v", contents, err)
	}
	state, err := Describe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Conflict || state.Owned {
		t.Fatalf("Describe = %#v", state)
	}
}

func TestUninstallRemovesOnlyProvenDefinitionAndIsIdempotent(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), env, "/usr/local/bin/grafo", stateDir, false); err != nil {
		t.Fatal(err)
	}
	path, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(context.Background(), env, false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("proven definition was not removed: %v", err)
	}
	if _, err := Uninstall(context.Background(), env, false); err != nil {
		t.Fatalf("replayed uninstall: %v", err)
	}
	// A definition Grafo cannot prove it wrote survives uninstall.
	if err := os.WriteFile(path, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err := Uninstall(context.Background(), env, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unproven definition was removed: %v", err)
	}
	last := actions[len(actions)-1]
	if last.Change != ChangeSkipped || last.Detail == "" {
		t.Fatalf("expected a reported skip, got %#v", last)
	}
}

// Finding 1: stopping a service is itself a mutation, so it must happen only
// after ownership is proven, never before.
func TestUninstallNeverTouchesAServiceItCannotProveItOwns(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	path, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentinstall.ParentPath("linux", path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "[Unit]\nDescription=A unit the user manages\n[Service]\nExecStart=/usr/bin/true\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err := Uninstall(context.Background(), env, false)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	env.mutex.Lock()
	commands := append([]string{}, env.commands...)
	env.mutex.Unlock()
	if len(commands) != 0 {
		t.Fatalf("uninstall ran commands against a unit it does not own: %#v", commands)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != foreign {
		t.Fatalf("foreign unit was modified: %q (%v)", contents, err)
	}
	if len(actions) != 1 || actions[0].Kind != "definition" || actions[0].Change != ChangeSkipped {
		t.Fatalf("actions = %#v", actions)
	}
	if !strings.Contains(actions[0].Detail, "left running untouched") {
		t.Fatalf("the report does not say the service was left alone: %#v", actions[0])
	}
}

// Finding 2: a receipt proves ownership of one location, not of an artifact kind.
func TestInstallIgnoresAReceiptRecordedForAnotherPath(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	path, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	foreign := "[Service]\nExecStart=/usr/bin/true\n"
	// The receipt claims the very same bytes, but at a different location.
	elsewhere, err := agentinstall.ConfigHomePath(env, "systemd", "user", "other-"+SystemdUnit)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentinstall.RecordOwnedFile(env, agentinstall.ServiceOwner, "systemd", elsewhere, resolvedUserTarget(t, env, elsewhere), foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentinstall.ParentPath("linux", path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), env, installedBinary(t), stateDir, false); err == nil {
		t.Fatal("a receipt for another path must not authorize replacing this one")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != foreign {
		t.Fatalf("definition was rewritten: %q (%v)", contents, err)
	}
	state, err := Describe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if state.Owned || !state.Conflict {
		t.Fatalf("Describe trusted a receipt for another path: %#v", state)
	}
}

func TestServiceReceiptDoesNotFollowRetargetedParent(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := Install(context.Background(), env, installedBinary(t), stateDir, false)
	if err != nil {
		t.Fatal(err)
	}
	path := actions[0].Target
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	original := parent + ".original"
	if err = os.Rename(parent, original); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(parent), "retargeted-user-units")
	if err = os.MkdirAll(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(replacement, filepath.Base(path)), generated, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(replacement, parent); err != nil {
		t.Fatal(err)
	}

	removed, err := Uninstall(context.Background(), env, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Change != ChangeSkipped {
		t.Fatalf("uninstall actions = %#v", removed)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != string(generated) {
		t.Fatalf("retargeted user definition changed: %q (%v)", contents, err)
	}
}

// Finding 5: ExecStart is a command line, so every path must survive quoting.
func TestSystemdArgumentQuotesEveryValueAndRefusesTheRest(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      string
		wantError bool
	}{
		{name: "plain path", value: "/usr/local/bin/grafo", want: `"/usr/local/bin/grafo"`},
		{name: "space", value: "/home/a b/bin/grafo", want: `"/home/a b/bin/grafo"`},
		{name: "tab is a control character", value: "/home/a\tb/grafo", wantError: true},
		{name: "specifier", value: "/home/50%/grafo", want: `"/home/50%%/grafo"`},
		{name: "double quote", value: `/home/a"b/grafo`, want: `"/home/a\"b/grafo"`},
		{name: "backslash", value: `/home/a\b/grafo`, want: `"/home/a\\b/grafo"`},
		{name: "semicolon", value: "/home/a;b/grafo", want: `"/home/a;b/grafo"`},
		{name: "dollar", value: "/home/$HOME/grafo", want: `"/home/$HOME/grafo"`},
		{name: "newline", value: "/home/a\nExecStart=/bin/sh", wantError: true},
		{name: "empty", value: "   ", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := systemdArgument(test.value)
			if test.wantError {
				if err == nil {
					t.Fatalf("systemdArgument(%q) = %q, want a refusal", test.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("systemdArgument(%q): %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("systemdArgument(%q) = %q, want %q", test.value, got, test.want)
			}
			// Whatever is emitted must parse back to the original path.
			parsed, ok := firstSystemdArgument(got + " service run")
			if !ok || parsed != test.value {
				t.Fatalf("round trip of %q gave %q (ok=%v)", test.value, parsed, ok)
			}
		})
	}
}

func TestGeneratedDefinitionsSurviveAwkwardPaths(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			env := newRecordingEnvironment(t, goos)
			binary := filepath.Join(t.TempDir(), "grafo tools", "grafo")
			if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(t.TempDir(), "state dir 100%")
			actions, err := Install(context.Background(), env, binary, stateDir, false)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			contents, err := os.ReadFile(actions[0].Target)
			if err != nil {
				t.Fatal(err)
			}
			declared, ok := DefinitionBinary(goos, string(contents))
			if !ok {
				t.Fatalf("could not parse the executable back out of:\n%s", contents)
			}
			if declared != binary {
				t.Fatalf("definition starts %q, want %q", declared, binary)
			}
			if goos == "linux" && !strings.Contains(string(contents), `ExecStart="`+binary+`" service run --state-dir "`+strings.ReplaceAll(stateDir, "%", "%%")+`"`) {
				t.Fatalf("systemd unit does not quote its arguments:\n%s", contents)
			}
		})
	}
}

func TestDefinitionRefusesAPathItCannotRepresent(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir := "/home/user/state\nExecStart=/bin/sh"
	if _, err := Install(context.Background(), env, installedBinary(t), stateDir, false); err == nil {
		t.Fatal("expected a refusal rather than a broken unit")
	}
	path, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused definition was still written: %v", err)
	}
}
