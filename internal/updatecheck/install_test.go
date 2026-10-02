package updatecheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// isolateInstallEnvironment clears everything Detect consults so a developer's
// own GOPATH or Homebrew prefix cannot decide a test.
func isolateInstallEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", filepath.Join(testtemp.Dir(t), "unused-gopath"))
}

func TestDetectClassifiesInstallLayouts(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		path        string
		environment map[string]string
		want        Method
	}{
		{
			name: "homebrew cask on apple silicon",
			path: "/opt/homebrew/Caskroom/grafo/0.4.2/grafo",
			want: MethodHomebrew,
		},
		{
			name: "homebrew cask on intel macos",
			path: "/usr/local/Caskroom/grafo/0.4.2/grafo",
			want: MethodHomebrew,
		},
		{
			name: "homebrew on linux",
			path: "/home/linuxbrew/.linuxbrew/bin/grafo",
			want: MethodHomebrew,
		},
		{
			name:        "homebrew at a relocated prefix",
			path:        "/custom/brew/bin/grafo",
			environment: map[string]string{"HOMEBREW_PREFIX": "/custom/brew"},
			want:        MethodHomebrew,
		},
		{
			name:        "go install into GOBIN",
			path:        "/Users/example/bin/grafo",
			environment: map[string]string{"GOBIN": "/Users/example/bin"},
			want:        MethodGoInstall,
		},
		{
			name:        "go install into GOPATH/bin",
			path:        "/Users/example/go/bin/grafo",
			environment: map[string]string{"GOPATH": "/Users/example/go"},
			want:        MethodGoInstall,
		},
		{
			name:        "go install into the second GOPATH entry",
			path:        "/second/root/bin/grafo",
			environment: map[string]string{"GOPATH": "/first/root" + string(os.PathListSeparator) + "/second/root"},
			want:        MethodGoInstall,
		},
		{
			name: "archive unpacked into a local bin",
			path: "/usr/local/bin/grafo",
			want: MethodArchive,
		},
		{
			name: "a directory that merely starts like the homebrew prefix",
			path: "/opt/homebrew-backup/bin/grafo",
			want: MethodArchive,
		},
		{
			name: "an unknown executable path",
			path: "",
			want: MethodArchive,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			isolateInstallEnvironment(t)
			for name, value := range testCase.environment {
				t.Setenv(name, value)
			}
			if got := Detect(testCase.path); got != testCase.want {
				t.Fatalf("Detect(%q) = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

// TestDetectResolvesSymlinks covers the layout Homebrew actually installs:
// <prefix>/bin/grafo is a symlink at a versioned file inside the Caskroom. An
// unresolved path reports every cask install as an archive install.
func TestDetectResolvesSymlinks(t *testing.T) {
	isolateInstallEnvironment(t)
	prefix := testtemp.Dir(t)
	t.Setenv("HOMEBREW_PREFIX", prefix)

	caskroom := filepath.Join(prefix, "Caskroom", "grafo", "0.4.2")
	if err := os.MkdirAll(caskroom, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(caskroom, "grafo")
	if err := os.WriteFile(target, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The link lives outside every known prefix, so only symlink resolution can
	// attribute it to Homebrew.
	linkDirectory := testtemp.Dir(t)
	link := filepath.Join(linkDirectory, "grafo")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := Detect(link); got != MethodHomebrew {
		t.Fatalf("Detect(%q) = %q, want %q", link, got, MethodHomebrew)
	}
}

func TestArchiveNameMatchesTheReleaseAsset(t *testing.T) {
	// The expected name mirrors the name_template in .goreleaser.yaml. A change
	// to either has to be made in both places.
	want := "grafo_0.4.2_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	for _, version := range []string{"0.4.2", "v0.4.2"} {
		if got := archiveName(version); got != want {
			t.Errorf("archiveName(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestHintNamesThePackageManagerCommand(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		method   Method
		want     string
		contains []string
	}{
		{name: "homebrew", method: MethodHomebrew, want: "brew upgrade --cask grafo"},
		{
			name:   "go install",
			method: MethodGoInstall,
			want:   "go install github.com/cafecito-games/grafo/cmd/grafo@latest",
		},
		{
			name:   "archive names the asset, the release and the file to replace",
			method: MethodArchive,
			contains: []string{
				archiveName("0.4.2"),
				"https://github.com/cafecito-games/grafo/releases/tag/v0.4.2",
				"/usr/local/bin/grafo",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hint := Hint(testCase.method, "v0.4.2", "/usr/local/bin/grafo")
			if testCase.want != "" && hint != testCase.want {
				t.Fatalf("Hint = %q, want %q", hint, testCase.want)
			}
			for _, fragment := range testCase.contains {
				if !strings.Contains(hint, fragment) {
					t.Errorf("Hint = %q, want it to mention %q", hint, fragment)
				}
			}
		})
	}
}

// TestArchiveHintWithoutAnExecutablePath covers the binary whose own location
// could not be resolved: the asset is still named, without an empty clause.
func TestArchiveHintWithoutAnExecutablePath(t *testing.T) {
	hint := Hint(MethodArchive, "0.4.2", "")
	if strings.Contains(hint, "replace") {
		t.Fatalf("Hint = %q, want no replacement clause without a known path", hint)
	}
	if !strings.Contains(hint, archiveName("0.4.2")) {
		t.Fatalf("Hint = %q, want it to name the release asset", hint)
	}
}

func TestNoticeStatesTheFactThenTheAction(t *testing.T) {
	notice := Notice("0.4.1", "v0.4.2", MethodHomebrew, "/opt/homebrew/bin/grafo")
	want := "grafo: 0.4.2 is available (you have 0.4.1)\n" +
		"grafo: update with: brew upgrade --cask grafo\n"
	if notice != want {
		t.Fatalf("Notice = %q, want %q", notice, want)
	}
}
