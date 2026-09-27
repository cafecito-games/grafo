package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/agentinstall"
)

// hostEnvironment is the production environment with an overridable operating
// system, so generated launchd and systemd definitions are testable from one
// host.
type hostEnvironment struct {
	agentinstall.OSEnvironment
	goos string
}

func (e hostEnvironment) GOOS() string {
	if e.goos == "" {
		return e.OSEnvironment.GOOS()
	}
	return e.goos
}

// isolatedEnvironment points HOME and XDG_CONFIG_HOME at a throwaway directory
// so no test can read or write the developer's real configuration.
func isolatedEnvironment(t *testing.T) agentinstall.Environment {
	return isolatedEnvironmentFor(t, "")
}

func isolatedEnvironmentFor(t *testing.T, goos string) agentinstall.Environment {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return hostEnvironment{goos: goos}
}

func mkdir(path string) error { return os.MkdirAll(path, 0o755) }

func removeAll(path string) error { return os.RemoveAll(path) }

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
