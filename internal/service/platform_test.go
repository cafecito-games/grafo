package service

import (
	"context"
	"os"
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
	isolatedEnvironmentFor(t, goos)
	return &recordingEnvironment{
		hostEnvironment: hostEnvironment{goos: goos},
		outputs:         map[string]string{},
		failures:        map[string]bool{},
	}
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
			if !agentinstall.ProvesFile(receipt, path, text) {
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
