package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgumentsAcceptsNewOptions(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		wantError string
		check     func(*testing.T, parsedArguments)
	}{
		{
			name:      "impact per-direction bounds",
			arguments: []string{"impact", "pkg.Fn", "--upstream-depth", "2", "--downstream-limit=50", "--source"},
			check: func(t *testing.T, args parsedArguments) {
				if args.values["upstream-depth"] != "2" || args.values["downstream-limit"] != "50" {
					t.Fatalf("unexpected values: %#v", args.values)
				}
				if !args.flags["source"] {
					t.Fatal("--source should be a boolean flag")
				}
			},
		},
		{
			name:      "search filters",
			arguments: []string{"search", "needle", "--regex", "--case-sensitive", "--path-prefix", "internal,cmd", "--language", "go"},
			check: func(t *testing.T, args parsedArguments) {
				if !args.flags["regex"] || !args.flags["case-sensitive"] {
					t.Fatalf("unexpected flags: %#v", args.flags)
				}
				if got := splitList(args.values["path-prefix"]); len(got) != 2 || got[0] != "internal" || got[1] != "cmd" {
					t.Fatalf("unexpected path prefixes: %#v", got)
				}
			},
		},
		{
			name:      "installer selection",
			arguments: []string{"install", "--client", "cursor,vscode", "--dry-run", "--json"},
			check: func(t *testing.T, args parsedArguments) {
				options := installTargets(args)
				if !options.DryRun || len(options.Targets) != 2 {
					t.Fatalf("unexpected options: %#v", options)
				}
			},
		},
		{
			name:      "boolean flags reject values",
			arguments: []string{"install", "--dry-run=yes"},
			wantError: "does not take a value",
		},
		{
			name:      "unknown option still fails",
			arguments: []string{"search", "needle", "--nope"},
			wantError: "unknown option",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, err := parseArguments(test.arguments)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("want error containing %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			test.check(t, args)
		})
	}
}

func TestInstallTargetsMergesPositionalsAndFlag(t *testing.T) {
	args, err := parseArguments([]string{"install", "claude", "--client", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	options := installTargets(args)
	if len(options.Targets) != 2 || options.Targets[0] != "claude" || options.Targets[1] != "cursor" {
		t.Fatalf("unexpected targets: %#v", options.Targets)
	}
	// --all supersedes explicit names so every supported client is considered.
	args, err = parseArguments([]string{"install", "claude", "--all"})
	if err != nil {
		t.Fatal(err)
	}
	options = installTargets(args)
	if !options.All || len(options.Targets) != 0 {
		t.Fatalf("--all should clear explicit targets: %#v", options)
	}
}

// indexedRepository writes a tiny Go module, indexes it, and returns its root.
func indexedRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module sample\n\ngo 1.26\n")
	write("charge.go", "package sample\n\n// Charge settles a payment.\nfunc Charge() error { return nil }\n")
	write("checkout.go", "package sample\n\nfunc Checkout() error {\n\treturn Charge()\n}\n")
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}
	return root
}

func run(t *testing.T, arguments ...string) int {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return New(&stdout, &stderr).Run(context.Background(), arguments)
}

func output(t *testing.T, arguments ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), arguments)
	return stdout.String(), stderr.String(), code
}

func TestImpactReportsBothDirections(t *testing.T) {
	root := indexedRepository(t)
	stdout, stderr, code := output(t, "impact", "sample.Charge", "--repo", root, "--depth", "2")
	if code != 0 {
		t.Fatalf("impact exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "upstream · depends on this") {
		t.Fatalf("missing upstream section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "downstream · this depends on") {
		t.Fatalf("missing downstream section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sample.Checkout") {
		t.Fatalf("Checkout calls Charge, so it belongs upstream:\n%s", stdout)
	}
	if !strings.Contains(stdout, "impacted files") {
		t.Fatalf("missing impacted files:\n%s", stdout)
	}

	// The alias keeps working and JSON carries both sections.
	stdout, stderr, code = output(t, "blast-radius", "sample.Charge", "--repo", root, "--json")
	if code != 0 {
		t.Fatalf("blast-radius exited with %d: %s", code, stderr)
	}
	for _, key := range []string{`"upstream"`, `"downstream"`, `"impacted_files"`, `"cross_repository_hops"`} {
		if !strings.Contains(stdout, key) {
			t.Fatalf("JSON report is missing %s:\n%s", key, stdout)
		}
	}

	// Source is opt-in.
	stdout, _, code = output(t, "impact", "sample.Charge", "--repo", root, "--depth", "1", "--source")
	if code != 0 || !strings.Contains(stdout, "func Charge()") {
		t.Fatalf("--source should include a bounded excerpt:\n%s", stdout)
	}
	stdout, _, _ = output(t, "impact", "sample.Charge", "--repo", root, "--depth", "1")
	if strings.Contains(stdout, "func Charge()") {
		t.Fatalf("source must stay opt-in:\n%s", stdout)
	}
}

func TestSearchFindsIndexedContentOnly(t *testing.T) {
	root := indexedRepository(t)

	stdout, stderr, code := output(t, "search", "settles a payment", "--repo", root)
	if code != 0 {
		t.Fatalf("search exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "charge.go:3") {
		t.Fatalf("expected the comment match:\n%s", stdout)
	}

	// A file that is not in the index is not searchable, even though it exists.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("settles a payment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, code = output(t, "search", "settles a payment", "--repo", root)
	if code != 0 {
		t.Fatalf("search exited with %d", code)
	}
	if strings.Contains(stdout, "notes.txt") {
		t.Fatalf("search must be bounded to indexed files:\n%s", stdout)
	}

	// Regex mode and caps are reported rather than silently truncating.
	stdout, stderr, code = output(t, "search", "func [A-Z]", "--regex", "--max-matches", "1", "--repo", root)
	if code != 0 {
		t.Fatalf("regex search exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "truncated") {
		t.Fatalf("expected a truncation marker:\n%s", stdout)
	}
	if !strings.Contains(stderr, "max_matches cap of 1") {
		t.Fatalf("expected a note naming the cap that clipped results:\n%s", stderr)
	}

	// An invalid pattern names the problem instead of searching.
	_, stderr, code = output(t, "search", "a(", "--regex", "--repo", root)
	if code == 0 || !strings.Contains(stderr, "a(") {
		t.Fatalf("invalid regex should fail and name the pattern: %q (code %d)", stderr, code)
	}
}

func TestHelpDocumentsNewCommands(t *testing.T) {
	stdout, _, code := output(t, "help")
	if code != 0 {
		t.Fatalf("help exited with %d", code)
	}
	for _, fragment := range []string{"grafo uninstall", "grafo search", "--upstream-depth", "--dry-run", "--list", "--kind"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("help does not document %q:\n%s", fragment, stdout)
		}
	}
}

func TestUnknownCommandStillFails(t *testing.T) {
	if code := run(t, "nope"); code != 1 {
		t.Fatalf("unknown command should exit 1, got %d", code)
	}
}

func TestInstallOptionsSelectArtifactKinds(t *testing.T) {
	args, err := parseArguments([]string{"install", "--all", "--mcp-only", "--hooks", "--refresh"})
	if err != nil {
		t.Fatal(err)
	}
	options := installTargets(args)
	if !options.MCPOnly || !options.Hooks || !options.Refresh {
		t.Fatalf("unexpected options: %#v", options)
	}
	// A dry run or JSON output already prints the full plan, so it is not
	// announced twice.
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	if app.announcer(args) == nil {
		t.Fatal("a real run must announce its plan before mutating")
	}
	dry, err := parseArguments([]string{"install", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if app.announcer(dry) != nil {
		t.Fatal("a dry run must not announce twice")
	}
}

func TestGuidancePrintsCanonicalTextAndIndexStatus(t *testing.T) {
	root := indexedRepository(t)
	stdout, _, code := output(t, "guidance", "--repo", root)
	if code != 0 {
		t.Fatalf("guidance exited with %d", code)
	}
	for _, fragment := range []string{"get_blast_radius", "find_reusable_code", "Index status: ready"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("guidance does not mention %q:\n%s", fragment, stdout)
		}
	}
}

func TestGuidanceReportsMissingIndex(t *testing.T) {
	stdout, _, code := output(t, "guidance", "--repo", t.TempDir())
	if code != 0 {
		t.Fatalf("guidance exited with %d", code)
	}
	if !strings.Contains(stdout, "Index status: none") {
		t.Fatalf("guidance did not report a missing index:\n%s", stdout)
	}
}

func TestGuidanceHooksAlwaysSucceed(t *testing.T) {
	// Advisory hooks must fail open: even an unindexed or unknown path, and an
	// unknown phase, exit 0 with usable context.
	for _, phase := range []string{"pre-search", "pre-edit", "unknown-phase"} {
		stdout, _, code := output(t, "guidance", "--hook", phase, "--repo", filepath.Join(t.TempDir(), "missing"))
		if code != 0 {
			t.Fatalf("hook %q exited with %d", phase, code)
		}
		if !strings.Contains(stdout, "Grafo advisory") {
			t.Fatalf("hook %q printed no advisory context: %s", phase, stdout)
		}
	}
}

func TestHelpDocumentsGuidanceInstallation(t *testing.T) {
	stdout, _, code := output(t, "help")
	if code != 0 {
		t.Fatalf("help exited with %d", code)
	}
	for _, fragment := range []string{"grafo guidance", "--mcp-only", "--refresh", "--hooks"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("help does not document %q:\n%s", fragment, stdout)
		}
	}
}

// TestShowReportsAmbiguityAndHonorsKind covers the CLI half of selector
// resolution: an ambiguous selector must fail with a non-zero status and list the
// candidates, and --kind must make it resolvable without guessing.
func TestShowReportsAmbiguityAndHonorsKind(t *testing.T) {
	root := indexedRepository(t)
	if err := os.WriteFile(filepath.Join(root, "request.go"),
		[]byte("package sample\n\ntype Request struct {\n\tCharge int\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}

	stdout, stderr, code := output(t, "show", "Charge", "--repo", root)
	if code == 0 {
		t.Fatalf("an ambiguous selector must not resolve silently:\n%s", stdout)
	}
	if !strings.Contains(stderr, "matches 2 nodes by name") {
		t.Fatalf("ambiguity must report the total and the reason:\n%s", stderr)
	}
	if !strings.Contains(stdout, "sample.Charge") || !strings.Contains(stdout, "sample.Request.Charge") {
		t.Fatalf("both candidates must be listed:\n%s", stdout)
	}

	stdout, stderr, code = output(t, "show", "Charge", "--repo", root, "--kind", "function")
	if code != 0 {
		t.Fatalf("kind-filtered show exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "sample.Charge") || strings.Contains(stdout, "Request.Charge") {
		t.Fatalf("--kind should select the function:\n%s", stdout)
	}

	if _, stderr, code = output(t, "show", "Charge", "--repo", root, "--kind", "nonsense"); code == 0 ||
		!strings.Contains(stderr, "unknown node kind") {
		t.Fatalf("an unknown kind must fail loudly, got %d: %s", code, stderr)
	}
}
