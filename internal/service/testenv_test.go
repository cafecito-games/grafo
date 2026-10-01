package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// hostEnvironment is the production environment with an overridable operating
// system, so generated launchd and systemd definitions are testable from one
// host.
type hostEnvironment struct {
	agentinstall.OSEnvironment
	goos string
	// temporaryDir overrides the reported temporary directory. Tests keep their
	// fixtures under testtemp.Dir(t), which really is inside the OS temporary
	// directory, so a test binary there must not be judged ephemeral by the rule
	// that protects production installs.
	temporaryDir string
}

func (e hostEnvironment) GOOS() string {
	if e.goos == "" {
		return e.OSEnvironment.GOOS()
	}
	return e.goos
}

func (e hostEnvironment) TempDir() string {
	if e.temporaryDir == "" {
		return e.OSEnvironment.TempDir()
	}
	return e.temporaryDir
}

// isolatedEnvironment points HOME and XDG_CONFIG_HOME at a throwaway directory
// so no test can read or write the developer's real configuration.
func isolatedEnvironment(t *testing.T) agentinstall.Environment {
	return isolatedEnvironmentFor(t, "")
}

func isolatedEnvironmentFor(t *testing.T, goos string) agentinstall.Environment {
	t.Helper()
	return isolatedHost(t, goos)
}

func isolatedHost(t *testing.T, goos string) hostEnvironment {
	t.Helper()
	home := testtemp.Dir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("GOTMPDIR", "")
	t.Setenv("GOCACHE", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	return hostEnvironment{goos: goos, temporaryDir: testtemp.Dir(t)}
}

func mkdir(path string) error { return os.MkdirAll(path, 0o755) }

func removeAll(path string) error { return os.RemoveAll(path) }

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
