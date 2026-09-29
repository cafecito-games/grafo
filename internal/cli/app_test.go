package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
)

func TestOpenReadUsesCapabilitySafeRepositoryAfterRefresh(t *testing.T) {
	root := indexedRepository(t)
	args, err := parseArguments([]string{"find", "Charge", "--repo", root})
	if err != nil {
		t.Fatal(err)
	}
	repository, projects, closeRepository, err := openRead(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRepository() }()
	if len(projects) != 1 {
		t.Fatalf("projects = %#v", projects)
	}
	if _, ok := repository.(graph.IndexRepository); ok {
		t.Fatal("query command retained index-write capability")
	}
	nodes, err := repository.SearchNodes(context.Background(), "Charge", 10)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("query-only search = %#v, %v", nodes, err)
	}
}

func TestMCPUsesCoordinatorRootsAndNotWritableOpenRead(t *testing.T) {
	single, err := parseArguments([]string{"mcp", "--repo", "/tmp/one"})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := mcpRoots(single)
	if err != nil || len(roots) != 1 || roots[0] != "/tmp/one" {
		t.Fatalf("single roots=%#v err=%v", roots, err)
	}
	federated, err := parseArguments([]string{"mcp", "--repos", "/tmp/two,/tmp/one"})
	if err != nil {
		t.Fatal(err)
	}
	roots, err = mcpRoots(federated)
	if err != nil || len(roots) != 2 || roots[0] != "/tmp/two" || roots[1] != "/tmp/one" {
		t.Fatalf("federated roots=%#v err=%v", roots, err)
	}
	if requiresWritableRead("mcp") {
		t.Fatal("MCP remained on the legacy persistent writable open path")
	}
}

func TestParseArgumentsAccumulatesRepeatablePathPrefixes(t *testing.T) {
	args, err := parseArguments([]string{"events", "--path-prefix", "internal/app", "--path-prefix=cmd,web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := args.values["path-prefix"]; got != "internal/app,cmd,web" {
		t.Fatalf("path-prefix = %q", got)
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil || len(prefixes) != 3 {
		t.Fatalf("normalized path prefixes = %#v, %v", prefixes, err)
	}
	for _, arguments := range [][]string{
		{"events", "--path-prefix", ""},
		{"events", "--path-prefix", "internal,,cmd"},
		{"events", "--path-prefix", "internal", "--path-prefix", ""},
	} {
		invalid, parseErr := parseArguments(arguments)
		if parseErr != nil {
			t.Fatalf("parse invalid transport option %v: %v", arguments, parseErr)
		}
		if _, err := pathPrefixOption(invalid); err == nil {
			t.Fatalf("blank path prefix was accepted for %v", arguments)
		}
	}
}

func TestRunRejectsPathPrefixForEveryUnsupportedCommandBeforeWork(t *testing.T) {
	for _, arguments := range [][]string{
		{"index", "--path-prefix", "internal"},
		{"path", "From", "To", "--path-prefix", "internal"},
		{"find-tests", "pkg.Symbol", "--path-prefix", "internal"},
		{"message-flow", "acme.Message", "--path-prefix", "internal"},
	} {
		var stdout, stderr bytes.Buffer
		code := New(&stdout, &stderr).Run(context.Background(), arguments)
		if code == 0 || !strings.Contains(stderr.String(), "--path-prefix is not supported") {
			t.Fatalf("grafo %v: code=%d stderr=%q", arguments, code, stderr.String())
		}
	}
}

func TestOpenReadUsesReadOnlyGraphForReusableAlias(t *testing.T) {
	root := indexedRepository(t)
	args, err := parseArguments([]string{"find-reusable-code", "payment helper", "--repo", root})
	if err != nil {
		t.Fatal(err)
	}
	repository, _, closeRepository, err := openRead(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRepository() }()
	if _, ok := repository.(graph.IndexRepository); ok {
		t.Fatal("find-reusable-code alias retained graph write capability")
	}
	if _, ok := repository.(semantic.CandidateRepository); !ok {
		t.Fatal("find-reusable-code alias lost semantic candidate capability")
	}
}

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

func TestFailureFlowReportsTypedEscapes(t *testing.T) {
	root := indexedRepository(t)
	stdout, stderr, code := output(t, "failure-flow", "sample.Checkout", "--repo", root)
	if code != 0 {
		t.Fatalf("failure-flow exited with %d: %s", code, stderr)
	}
	for _, expected := range []string{"error returns", "escaping failures", "sample.Charge", "returns_error", "propagates_error"} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("missing %q in failure-flow output:\n%s", expected, stdout)
		}
	}
}

func TestTestCoverageCommandsExposeStructuralEvidence(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module sample\n\ngo 1.26\n")
	write("charge.go", "package sample\nfunc Charge() {}\n")
	write("charge_test.go", `package sample
import "testing"
func TestCharge(t *testing.T) { helper() }
func helper() { Charge() }
`)
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}
	stdout, stderr, code := output(t, "find-tests", "sample.Charge", "--repo", root)
	if code != 0 || !strings.Contains(stdout, "sample.TestCharge") || !strings.Contains(stdout, "helper-expanded") ||
		!strings.Contains(stdout, "not runtime coverage") {
		t.Fatalf("find-tests output: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	stdout, stderr, code = output(t, "get_test_coverage", "sample.TestCharge", "--repo", root, "--json")
	if code != 0 || !strings.Contains(stdout, `"designation": "structural"`) || !strings.Contains(stdout, `"qualified_name": "sample.Charge"`) {
		t.Fatalf("test-coverage JSON: code=%d stdout=%s stderr=%s", code, stdout, stderr)
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

// TestShowPrefersTableOverItsOwnColumn is the SQL half of declaration-member
// suppression, end to end through the real parser: "CREATE TABLE charge (charge
// TEXT)" produces a table named charge and a column named charge whose qualified
// name extends it, and the selector must name the table rather than reporting them
// as rivals. The column stays reachable by kind and by qualified name.
func TestShowPrefersTableOverItsOwnColumn(t *testing.T) {
	root := t.TempDir()
	// The statement is valid in both dialects, so name the dialect explicitly
	// rather than letting the router report ambiguous syntax.
	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"),
		[]byte("sql:\n  default_dialect: sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schema.sql"),
		[]byte("CREATE TABLE charge (charge TEXT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}

	stdout, stderr, code := output(t, "show", "charge", "--repo", root)
	if code != 0 {
		t.Fatalf("a table must win over its own column, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "table") || strings.Contains(stdout, "column") {
		t.Fatalf("expected the table, got:\n%s", stdout)
	}

	stdout, stderr, code = output(t, "show", "charge", "--repo", root, "--kind", "column")
	if code != 0 {
		t.Fatalf("kind-filtered show exited with %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "column") || !strings.Contains(stdout, "charge.charge") {
		t.Fatalf("--kind column should select the column, got:\n%s", stdout)
	}

	stdout, stderr, code = output(t, "show", "charge.charge", "--repo", root)
	if code != 0 {
		t.Fatalf("a column's qualified name must resolve, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "column") {
		t.Fatalf("expected the column, got:\n%s", stdout)
	}
}

// TestShowReportsAmbiguousSceneNodeHierarchy is the Godot half of the suppression
// invariant, end to end through the real .tscn parser. Scene nodes are modelled as
// hierarchical graph.KindVariable nodes, so a child sharing its parent's name
// produces two equally strong exact-name matches. Both are declarations GDScript
// can reference by name, so the selector must be ambiguous rather than silently
// resolving to the parent.
func TestShowReportsAmbiguousSceneNodeHierarchy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scenes"), 0o755); err != nil {
		t.Fatal(err)
	}
	scene := "[gd_scene format=3]\n\n[node name=\"Root\" type=\"Node2D\"]\n\n" +
		"[node name=\"Player\" type=\"Node2D\" parent=\".\"]\n\n" +
		"[node name=\"Player\" type=\"Sprite2D\" parent=\"Player\"]\n"
	if err := os.WriteFile(filepath.Join(root, "scenes", "main.tscn"), []byte(scene), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}

	stdout, stderr, code := output(t, "show", "Player", "--repo", root)
	if code == 0 {
		t.Fatalf("a child scene node sharing its parent's name must not resolve silently:\n%s", stdout)
	}
	if !strings.Contains(stderr, "matches 2 nodes by name") {
		t.Fatalf("expected both scene nodes to be reported:\n%s", stderr)
	}
	for _, qualified := range []string{"scenes/main:Root/Player", "scenes/main:Root/Player/Player"} {
		if !strings.Contains(stdout, qualified) {
			t.Fatalf("candidate %s is missing:\n%s", qualified, stdout)
		}
	}

	// Each node stays reachable by its own qualified name.
	stdout, stderr, code = output(t, "show", "scenes/main:Root/Player/Player", "--repo", root)
	if code != 0 {
		t.Fatalf("a scene node's qualified name must resolve, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "scenes/main:Root/Player/Player") {
		t.Fatalf("expected the child scene node:\n%s", stdout)
	}
}
