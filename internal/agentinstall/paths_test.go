package agentinstall

import (
	"context"
	"testing"
)

// fileAdapterByName returns the file-backed adapter registered under name.
func fileAdapterByName(t *testing.T, name string) fileAdapter {
	t.Helper()
	for _, entry := range registry {
		if entry.client().Name != name {
			continue
		}
		adapted, ok := entry.(fileAdapter)
		if !ok {
			t.Fatalf("client %q is not file-backed", name)
		}
		return adapted
	}
	t.Fatalf("unknown client %q", name)
	return fileAdapter{}
}

func TestFileAdapterConfigPathsPerPlatform(t *testing.T) {
	platforms := []struct {
		goos string
		home string
		vars map[string]string
		want map[string]string
	}{
		{
			goos: "linux",
			home: "/home/u",
			want: map[string]string{
				"claude-desktop": "/home/u/.config/Claude/claude_desktop_config.json",
				"cline":          "/home/u/.config/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json",
				"cursor":         "/home/u/.cursor/mcp.json",
				"gemini":         "/home/u/.gemini/settings.json",
				"vscode":         "/home/u/.config/Code/User/mcp.json",
				"windsurf":       "/home/u/.codeium/windsurf/mcp_config.json",
			},
		},
		{
			goos: "darwin",
			home: "/Users/u",
			want: map[string]string{
				"claude-desktop": "/Users/u/Library/Application Support/Claude/claude_desktop_config.json",
				"cline":          "/Users/u/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json",
				"cursor":         "/Users/u/.cursor/mcp.json",
				"gemini":         "/Users/u/.gemini/settings.json",
				"vscode":         "/Users/u/Library/Application Support/Code/User/mcp.json",
				"windsurf":       "/Users/u/.codeium/windsurf/mcp_config.json",
			},
		},
		{
			goos: "windows",
			home: `C:\Users\u`,
			vars: map[string]string{"APPDATA": `C:\Users\u\AppData\Roaming`},
			want: map[string]string{
				"claude-desktop": `C:\Users\u\AppData\Roaming\Claude\claude_desktop_config.json`,
				"cline":          `C:\Users\u\AppData\Roaming\Code\User\globalStorage\saoudrizwan.claude-dev\settings\cline_mcp_settings.json`,
				"cursor":         `C:\Users\u\.cursor\mcp.json`,
				"gemini":         `C:\Users\u\.gemini\settings.json`,
				"vscode":         `C:\Users\u\AppData\Roaming\Code\User\mcp.json`,
				"windsurf":       `C:\Users\u\.codeium\windsurf\mcp_config.json`,
			},
		},
	}

	for _, platform := range platforms {
		t.Run(platform.goos, func(t *testing.T) {
			environment := newFakeEnvironment(platform.goos, platform.home)
			for key, value := range platform.vars {
				environment.vars[key] = value
			}
			if len(platform.want) == 0 {
				t.Fatal("no expectations")
			}
			fileBacked := 0
			for _, entry := range registry {
				if _, ok := entry.(fileAdapter); ok {
					fileBacked++
				}
			}
			if fileBacked != len(platform.want) {
				t.Fatalf("file-backed adapters = %d, expectations = %d", fileBacked, len(platform.want))
			}
			for name, want := range platform.want {
				got, err := fileAdapterByName(t, name).configPath(environment)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got != want {
					t.Errorf("%s path = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestConfigPathHonorsEnvironmentOverrides(t *testing.T) {
	t.Run("xdg", func(t *testing.T) {
		environment := newFakeEnvironment("linux", "/home/u")
		environment.vars["XDG_CONFIG_HOME"] = "/home/u/cfg"
		got, err := fileAdapterByName(t, "vscode").configPath(environment)
		if err != nil {
			t.Fatal(err)
		}
		if want := "/home/u/cfg/Code/User/mcp.json"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("windows-without-appdata", func(t *testing.T) {
		environment := newFakeEnvironment("windows", `C:\Users\u`)
		got, err := fileAdapterByName(t, "claude-desktop").configPath(environment)
		if err != nil {
			t.Fatal(err)
		}
		if want := `C:\Users\u\AppData\Roaming\Claude\claude_desktop_config.json`; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("missing-home", func(t *testing.T) {
		environment := newFakeEnvironment("linux", "")
		if _, err := fileAdapterByName(t, "cursor").locate(context.Background(), environment); err == nil {
			t.Fatal("expected an error when the home directory is unavailable")
		}
	})
}

func TestParentPath(t *testing.T) {
	cases := []struct {
		goos string
		path string
		want string
	}{
		{goos: "linux", path: "/home/u/.cursor/mcp.json", want: "/home/u/.cursor"},
		{goos: "darwin", path: "/Users/u/Library/Application Support/Claude/c.json", want: "/Users/u/Library/Application Support/Claude"},
		{goos: "windows", path: `C:\Users\u\.cursor\mcp.json`, want: `C:\Users\u\.cursor`},
		{goos: "linux", path: "mcp.json", want: ""},
	}
	for _, test := range cases {
		if got := parentPath(test.goos, test.path); got != test.want {
			t.Errorf("parentPath(%q, %q) = %q, want %q", test.goos, test.path, got, test.want)
		}
	}
}
