package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestOptionalLockStatusMatchesDefaultPorcelainEvidence(t *testing.T) {
	root := testtemp.Dir(t)
	snapshotRunGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "staged.snap"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "renamed.snap"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotRunGit(t, root, "add", ".")
	snapshotRunGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "staged.snap"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotRunGit(t, root, "add", "staged.snap")
	if err := os.Rename(filepath.Join(root, "renamed.snap"), filepath.Join(root, "moved.snap")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.snap"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all"}
	optimized, err := (execGitRunner{}).Run(context.Background(), root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	baseline, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(optimized, baseline) {
		t.Fatalf("optional-lock porcelain differs:\noptimized %q\nbaseline  %q", optimized, baseline)
	}
}

func snapshotRunGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

type recordedGitCall struct {
	directory string
	arguments []string
}

type scriptedGitRunner struct {
	outputs [][]byte
	errors  []error
	calls   []recordedGitCall
}

type snapshotTestParser struct{}

func (snapshotTestParser) Language() string          { return "snapshot-test" }
func (snapshotTestParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (snapshotTestParser) Parse(context.Context, parserapi.Input) (graph.ParseResult, error) {
	return graph.ParseResult{}, nil
}

func (r *scriptedGitRunner) Run(_ context.Context, directory string, arguments ...string) ([]byte, error) {
	r.calls = append(r.calls, recordedGitCall{directory: directory, arguments: append([]string(nil), arguments...)})
	index := len(r.calls) - 1
	if index < len(r.errors) && r.errors[index] != nil {
		return nil, r.errors[index]
	}
	return r.outputs[index], nil
}

func TestDiscoverProjectSnapshotUsesThreeBoundedGitCommands(t *testing.T) {
	root := testtemp.Dir(t)
	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedGitRunner{outputs: [][]byte{
		[]byte(root + "\n"),
		[]byte("file:" + config + "\x00git@example.com:team/repository.git\x00"),
		[]byte("# branch.oid 0123456789012345678901234567890123456789\x00# branch.head main\x00"),
	}}

	project, err := discoverProject(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	if project.Branch != "main" || project.Commit != "0123456789012345678901234567890123456789" {
		t.Fatalf("unexpected project identity: %#v", project)
	}
	want := []recordedGitCall{
		{directory: root, arguments: []string{"rev-parse", "--show-toplevel"}},
		{directory: root, arguments: []string{"config", "--null", "--show-origin", "--get", "remote.origin.url"}},
		{directory: root, arguments: []string{"status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("git calls = %#v, want %#v", runner.calls, want)
	}
	if project.gitSnapshot == nil || !project.gitSnapshot.MembershipStable {
		t.Fatalf("clean snapshot did not prove stable membership: %#v", project.gitSnapshot)
	}
	if filepath.Base(project.IndexPath) != "main-0d6e4079e3.sqlite" {
		t.Fatalf("unexpected index path: %s", project.IndexPath)
	}
}

func TestDiscoverProjectReusesUnchangedRemoteIdentity(t *testing.T) {
	root := testtemp.Dir(t)
	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := &scriptedGitRunner{outputs: [][]byte{
		[]byte(root + "\n"),
		[]byte("file:" + config + "\x00git@example.com:team/cached.git\x00"),
		[]byte("# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00# branch.head main\x00"),
	}}
	project, err := discoverProject(context.Background(), root, first)
	if err != nil {
		t.Fatal(err)
	}
	second := &scriptedGitRunner{outputs: [][]byte{
		[]byte(root + "\n"),
		[]byte("# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00# branch.head main\x00"),
	}}
	reused, err := discoverProject(context.Background(), root, second)
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != reused.ID || len(second.calls) != 2 || reused.gitSnapshot.Commands != 2 {
		t.Fatalf("identity was not reused: first=%#v second=%#v calls=%#v", project, reused, second.calls)
	}
}

func TestParseGitStatusPorcelainV2CoversPathKinds(t *testing.T) {
	raw := []byte("# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00" +
		"# branch.head feature/work\x00" +
		"1 M. N... 100644 100644 100644 abc def staged.go\x00" +
		"1 .M N... 100644 100644 100644 abc def unstaged space.go\x00" +
		"1 D. N... 100644 000000 000000 abc 000 deleted.go\x00" +
		"2 R. N... 100644 100644 100644 abc def R100 renamed.go\x00old.go\x00" +
		"? untracked space.go\x00")

	snapshot, err := parseGitStatusPorcelainV2(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Branch != "feature/work" || snapshot.Head != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected branch snapshot: %#v", snapshot)
	}
	wantPaths := []string{"deleted.go", "old.go", "renamed.go", "staged.go", "unstaged space.go", "untracked space.go"}
	if !reflect.DeepEqual(snapshot.Changed, wantPaths) || !reflect.DeepEqual(snapshot.Dirty, wantPaths) {
		t.Fatalf("paths changed=%q dirty=%q, want %q", snapshot.Changed, snapshot.Dirty, wantPaths)
	}
	if snapshot.MembershipStable {
		t.Fatal("membership-changing records were reported stable")
	}
}

func TestParseGitStatusPorcelainV2FailsClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "missing rename source", raw: []byte("# branch.oid a\x002 R. N... 100644 100644 100644 abc def R100 new.go\x00")},
		{name: "unknown record", raw: []byte("# branch.oid a\x00x mystery\x00")},
		{name: "missing head", raw: []byte("# branch.head main\x00")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseGitStatusPorcelainV2(test.raw); err == nil {
				t.Fatal("malformed status was accepted")
			}
		})
	}
}

func TestParseGitStatusPorcelainV2TreatsTypechangeAsMembershipChange(t *testing.T) {
	snapshot, err := parseGitStatusPorcelainV2([]byte(
		"# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00# branch.head main\x00" +
			"1 .T N... 100644 100644 120000 abc def linked.snap\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MembershipStable {
		t.Fatal("tracked typechange incorrectly proved stable membership")
	}
}

func TestDiscoverProjectFallsBackToRootForEmptyRemote(t *testing.T) {
	root := testtemp.Dir(t)
	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedGitRunner{outputs: [][]byte{
		[]byte(root + "\n"),
		[]byte("file:" + config + "\x00\x00"),
		[]byte("# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00# branch.head main\x00"),
	}}
	project, err := discoverProject(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != graph.StableID("repo", root) {
		t.Fatalf("empty remote identity = %s, want root-derived identity", project.ID)
	}
}

func TestDiscoverProjectRejectsMalformedRemoteOrigin(t *testing.T) {
	root := testtemp.Dir(t)
	runner := &scriptedGitRunner{outputs: [][]byte{[]byte(root + "\n"), []byte("malformed")}}
	if _, err := discoverProject(context.Background(), root, runner); err == nil {
		t.Fatal("malformed remote origin was accepted")
	}
}

func TestDiscoverProjectUsesGitDetachedAbbreviation(t *testing.T) {
	root := testtemp.Dir(t)
	runner := &scriptedGitRunner{
		outputs: [][]byte{
			[]byte(root + "\n"), nil,
			[]byte("# branch.oid aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaabbbb\x00# branch.head (detached)\x00"),
			[]byte("aaaaaaaaaaaab\n"),
		},
		errors: []error{nil, errors.New("no remote"), nil, nil},
	}
	project, err := discoverProject(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	if project.Branch != "detached-aaaaaaaaaaaab" || project.gitSnapshot.Commands != 4 {
		t.Fatalf("detached project = %#v", project)
	}
	want := recordedGitCall{directory: root, arguments: []string{"rev-parse", "--short=12", "HEAD"}}
	if !reflect.DeepEqual(runner.calls[3], want) {
		t.Fatalf("detached abbreviation call = %#v, want %#v", runner.calls[3], want)
	}
}

func TestDiscoverProjectPropagatesCanceledGitProbe(t *testing.T) {
	root := testtemp.Dir(t)
	runner := &scriptedGitRunner{
		outputs: [][]byte{[]byte(root + "\n"), nil, nil},
		errors:  []error{nil, errors.New("no remote"), context.Canceled},
	}
	if _, err := discoverProject(context.Background(), root, runner); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestDetectGitChangesUsesOneExactHeadDiff(t *testing.T) {
	runner := &scriptedGitRunner{outputs: [][]byte{[]byte("committed.go\x00")}}
	snapshot := &GitSnapshot{
		Root: "/repository", Head: "new-head", Branch: "main",
		Changed: []string{"working.go"}, Dirty: []string{"working.go"}, runner: runner,
	}
	changes, err := detectGitChanges(context.Background(), "/repository", "old-head", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	wantCall := recordedGitCall{directory: "/repository", arguments: []string{"diff", "--name-only", "-z", "old-head", "new-head", "--"}}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], wantCall) {
		t.Fatalf("git calls = %#v, want %#v", runner.calls, wantCall)
	}
	if want := []string{"committed.go", "working.go"}; !reflect.DeepEqual(changes.changed, want) {
		t.Fatalf("changed = %q, want %q", changes.changed, want)
	}
	if changes.gitCommands != 1 {
		t.Fatalf("git command count = %d, want 1", changes.gitCommands)
	}
}

func TestDiscoverFilesUsesInjectedExactMembershipCommand(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "file.snap"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedGitRunner{outputs: [][]byte{[]byte("file.snap\x00")}}
	project := Project{Root: root, GitManaged: true, gitSnapshot: &GitSnapshot{runner: runner}}
	discovered, err := discoverFiles(context.Background(), project, parserapi.NewRegistry(snapshotTestParser{}))
	if err != nil {
		t.Fatal(err)
	}
	wantCall := recordedGitCall{directory: root, arguments: []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], wantCall) {
		t.Fatalf("git calls = %#v, want %#v", runner.calls, wantCall)
	}
	if !reflect.DeepEqual(discovered.paths, []string{"file.snap"}) || discovered.gitCommands != 1 {
		t.Fatalf("discovered = %#v", discovered)
	}
}

func TestDiscoverFilesExcludesManagedWorktrees(t *testing.T) {
	root := testtemp.Dir(t)
	for _, path := range []string{"kept.snap", ".worktrees/other/ignored.snap"} {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &scriptedGitRunner{outputs: [][]byte{[]byte("kept.snap\x00.worktrees/other/ignored.snap\x00")}}
	project := Project{Root: root, GitManaged: true, gitSnapshot: &GitSnapshot{runner: runner}}
	discovered, err := discoverFiles(context.Background(), project, parserapi.NewRegistry(snapshotTestParser{}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discovered.paths, []string{"kept.snap"}) {
		t.Fatalf("discovered paths = %q, want only repository-owned file", discovered.paths)
	}
}

func TestDiscoverFilesDiagnosesGitMembershipFallback(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "fallback.snap"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedGitRunner{errors: []error{errors.New("membership unavailable")}}
	project := Project{Root: root, GitManaged: true, gitSnapshot: &GitSnapshot{runner: runner}}
	discovered, err := discoverFiles(context.Background(), project, parserapi.NewRegistry(snapshotTestParser{}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discovered.paths, []string{"fallback.snap"}) {
		t.Fatalf("fallback paths = %q", discovered.paths)
	}
	if len(discovered.diagnostics) != 1 || !strings.Contains(discovered.diagnostics[0].Message, "membership unavailable") {
		t.Fatalf("fallback diagnostics = %#v", discovered.diagnostics)
	}
}

func TestDiscoverFilesRejectsSymlinkedAncestorTraversal(t *testing.T) {
	root := testtemp.Dir(t)
	outside := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(outside, "outside.snap"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedGitRunner{outputs: [][]byte{[]byte("linked/outside.snap\x00")}}
	project := Project{Root: root, GitManaged: true, gitSnapshot: &GitSnapshot{runner: runner}}
	discovered, err := discoverFiles(context.Background(), project, parserapi.NewRegistry(snapshotTestParser{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered.paths) != 0 {
		t.Fatalf("discovery followed symlinked ancestor: %q", discovered.paths)
	}
	if !reflect.DeepEqual(discovered.skipped, []string{"linked/outside.snap"}) {
		t.Fatalf("unsafe path was not reported as skipped: %q", discovered.skipped)
	}
}
