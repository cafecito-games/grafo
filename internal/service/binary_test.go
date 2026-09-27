package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// Finding 4: a service definition outlives the command that wrote it, so the
// binary it names must live somewhere durable. The temporary directory and the
// Go build cache are decided by containment in a directory the environment
// reports, not by matching a name. The one name rule matches only the shape the
// toolchain creates, because refusing a valid installed location is as much a
// defect as accepting an ephemeral one: both directions are asserted here.
func TestInstallableBinaryAcceptsDurablePathsAndRefusesEphemeralOnes(t *testing.T) {
	temporary := t.TempDir()
	goTemp := t.TempDir()
	goCache := t.TempDir()
	home := t.TempDir()
	cacheHome := filepath.Join(home, ".cache")
	durable := t.TempDir()

	tests := []struct {
		name      string
		goos      string
		path      string
		wantError string
		// noRemedy marks the one refusal that is not about location and therefore
		// has no install remedy to offer.
		noRemedy bool
	}{
		{
			name: "inside the temporary directory", goos: "linux",
			path:      filepath.Join(temporary, "go-build123", "b001", "exe", "grafo"),
			wantError: "temporary directory",
		},
		{
			name: "directly inside the temporary directory", goos: "linux",
			path: filepath.Join(temporary, "grafo"), wantError: "temporary directory",
		},
		{
			name: "inside the Go temporary directory", goos: "linux",
			path: filepath.Join(goTemp, "b001", "exe", "grafo"), wantError: "Go temporary directory",
		},
		{
			name: "inside an explicit GOCACHE", goos: "linux",
			path: filepath.Join(goCache, "ab", "grafo"), wantError: "Go build cache",
		},
		{
			name: "inside the default Linux build cache", goos: "linux",
			path: filepath.Join(cacheHome, "go-build", "ab", "exe", "grafo"), wantError: "Go build cache",
		},
		{
			name: "inside the default macOS build cache", goos: "darwin",
			path:      filepath.Join(home, "Library", "Caches", "go-build", "ab", "grafo"),
			wantError: "Go build cache",
		},
		{
			name: "a relocated build directory outside every known root", goos: "linux",
			path:      filepath.Join(durable, "go-build987654", "b001", "exe", "grafo"),
			wantError: "Go build directory",
		},
		{
			name: "a relocated build directory named with many digits", goos: "linux",
			path:      filepath.Join(durable, "go-build1234567890", "b001", "exe", "grafo"),
			wantError: "Go build directory",
		},
		// The fallback matches the shape the toolchain creates, so a durable prefix
		// that merely begins with those characters must still install.
		{
			name: "a durable directory named go-builder", goos: "linux",
			path: filepath.Join(durable, "opt", "go-builder", "bin", "grafo"),
		},
		{
			name: "a durable directory named go-build-tools", goos: "linux",
			path: filepath.Join(durable, "go-build-tools", "bin", "grafo"),
		},
		{
			name: "a durable directory named exactly go-build", goos: "linux",
			path: filepath.Join(durable, "srv", "go-build", "bin", "grafo"),
		},
		{
			name: "a durable directory named go-build2x", goos: "linux",
			path: filepath.Join(durable, "go-build2x", "bin", "grafo"),
		},
		{
			name: "an installed binary", goos: "linux",
			path: filepath.Join(durable, ".local", "bin", "grafo"),
		},
		{
			name: "an installed binary whose directory merely mentions go", goos: "linux",
			path: filepath.Join(durable, "gopath", "bin", "grafo"),
		},
		{name: "empty", goos: "linux", path: "   ", wantError: "empty path", noRemedy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", cacheHome)
			t.Setenv("GOTMPDIR", goTemp)
			t.Setenv("GOCACHE", goCache)
			environment := hostEnvironment{goos: test.goos, temporaryDir: temporary}
			resolved, err := InstallableBinary(environment, test.path)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("InstallableBinary(%q) = %v, want it accepted", test.path, err)
				}
				if resolved == "" {
					t.Fatal("an accepted binary must resolve to a path")
				}
				return
			}
			if err == nil {
				t.Fatalf("InstallableBinary(%q) was accepted, want a refusal", test.path)
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error %q does not name %q", err, test.wantError)
			}
			if !test.noRemedy && !strings.Contains(err.Error(), "go install") {
				t.Fatalf("error %q does not name the remedy", err)
			}
		})
	}
}
