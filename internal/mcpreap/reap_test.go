package mcpreap

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeProcess is one process a fabricated procfs reports.
type fakeProcess struct {
	pid        int
	arguments  []string
	parent     int
	workingDir string
	// rawCommand replaces the NUL-separated argv when a test needs a malformed
	// cmdline, and unreadable marks an entry whose files deny access.
	startTime   int64
	rawCommand  string
	malformed   bool
	unreadable  bool
	missingStat bool
	noWorkingD  bool
}

// fakeProcFS serves the three procfs files the scanner reads. It is a map
// rather than a real /proc so selection is tested against process shapes that
// cannot be staged safely on a developer's machine.
type fakeProcFS struct {
	processes []fakeProcess
	extra     []string
}

func (f fakeProcFS) find(name string) (fakeProcess, bool) {
	directory, _, _ := strings.Cut(name, "/")
	for _, process := range f.processes {
		if directory == strconv.Itoa(process.pid) {
			return process, true
		}
	}
	return fakeProcess{}, false
}

func (f fakeProcFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	mapped := fstest.MapFS{}
	for _, process := range f.processes {
		mapped[strconv.Itoa(process.pid)+"/cmdline"] = &fstest.MapFile{Mode: 0o444}
	}
	for _, name := range f.extra {
		mapped[name+"/placeholder"] = &fstest.MapFile{Mode: 0o444}
	}
	return mapped.ReadDir(".")
}

func (f fakeProcFS) ReadFile(name string) ([]byte, error) {
	process, ok := f.find(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	if process.unreadable {
		return nil, fs.ErrPermission
	}
	switch path.Base(name) {
	case "cmdline":
		if process.malformed {
			return []byte(process.rawCommand), nil
		}
		return []byte(strings.Join(process.arguments, "\x00") + "\x00"), nil
	case "status":
		if process.missingStat {
			return nil, fs.ErrNotExist
		}
		return []byte("Name:\tgrafo\nState:\tS (sleeping)\nPPid:\t" + strconv.Itoa(process.parent) + "\nThreads:\t18\n"), nil
	case "stat":
		if process.missingStat {
			return nil, fs.ErrNotExist
		}
		return []byte(strconv.Itoa(process.pid) + " (grafo) S " + strconv.Itoa(process.parent) +
			" 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 18 0 " +
			strconv.FormatInt(process.startTime, 10) + " 0 0\n"), nil
	}
	return nil, fs.ErrNotExist
}

func (f fakeProcFS) ReadLink(name string) (string, error) {
	process, ok := f.find(name)
	if !ok {
		return "", fs.ErrNotExist
	}
	if process.unreadable || process.noWorkingD {
		return "", fs.ErrPermission
	}
	if path.Base(name) == "cwd" {
		return process.workingDir, nil
	}
	return "", fs.ErrNotExist
}

// scannerOver builds a scanner over fabricated processes, where only the paths
// listed as present still exist on disk.
func scannerOver(processes []fakeProcess, self int, present ...string) (Scanner, *[]int) {
	var signalled []int
	scanner := Scanner{
		Proc:   fakeProcFS{processes: processes},
		Self:   self,
		Exists: func(candidate string) bool { return slices.Contains(present, candidate) },
		Signal: func(pid int) error {
			signalled = append(signalled, pid)
			return nil
		},
	}
	return scanner, &signalled
}

const liveClient = 4242

func TestReapSignalsAServerWhoseClientExited(t *testing.T) {
	scanner, signalled := scannerOver([]fakeProcess{
		{pid: 100, arguments: []string{"/home/u/.local/bin/grafo", "mcp", "--repo", "/repo"}, parent: 1, workingDir: "/repo"},
	}, 1, "/repo")

	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 1 || results[0].PID != 100 || results[0].Reason != ReasonClientExited {
		t.Fatalf("expected the orphan selected as %q, got %+v", ReasonClientExited, results)
	}
	if !results[0].Signalled {
		t.Fatalf("expected the orphan to be signalled: %+v", results[0])
	}
	if !slices.Equal(*signalled, []int{100}) {
		t.Fatalf("expected pid 100 signalled, got %v", *signalled)
	}
}

func TestReapLeavesAServerThatStillHasAClient(t *testing.T) {
	scanner, signalled := scannerOver([]fakeProcess{
		{pid: 100, arguments: []string{"/usr/bin/grafo", "mcp", "--repo", "/repo"}, parent: liveClient, workingDir: "/repo"},
	}, 1, "/repo")

	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected a server with a live client to be left alone, got %+v", results)
	}
	if len(*signalled) != 0 {
		t.Fatalf("expected no signals, got %v", *signalled)
	}
}

func TestReapSelectionRules(t *testing.T) {
	cases := []struct {
		name     string
		process  fakeProcess
		present  []string
		selected bool
		reason   Reason
	}{
		{
			name:     "every configured root is gone",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repos", "/gone/a,/gone/b"}, parent: liveClient, workingDir: "/gone/a"},
			selected: true,
			reason:   ReasonRootsRemoved,
		},
		{
			name:     "one of several roots is gone",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repos", "/live,/gone"}, parent: liveClient, workingDir: "/live"},
			present:  []string{"/live"},
			selected: false,
		},
		{
			name:     "the only root is gone, spelled with an equals sign",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repo=/gone"}, parent: liveClient, workingDir: "/gone"},
			selected: true,
			reason:   ReasonRootsRemoved,
		},
		{
			name:     "a relative root resolves against the server's working directory",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repo", "."}, parent: liveClient, workingDir: "/live"},
			present:  []string{"/live"},
			selected: false,
		},
		{
			name:     "the working directory was deleted",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: liveClient, workingDir: "/worktree/issue-1 (deleted)"},
			selected: true,
			reason:   ReasonWorkingDirectoryDeleted,
		},
		{
			name:     "a live server with no configured roots",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: liveClient, workingDir: "/live"},
			present:  []string{"/live"},
			selected: false,
		},
		{
			name:     "another program entirely",
			process:  fakeProcess{pid: 100, arguments: []string{"/usr/bin/node", "mcp"}, parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "grafo running a different subcommand",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "watch", "--repo", "/repo"}, parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "a program whose name merely ends in grafo",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/not-grafo", "mcp"}, parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "an mcp invocation whose first argument is a flag",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "--repo", "/repo", "mcp"}, parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "an empty command line",
			process:  fakeProcess{pid: 100, malformed: true, rawCommand: "", parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "a command line with no separators",
			process:  fakeProcess{pid: 100, malformed: true, rawCommand: "grafo mcp", parent: 1, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "an unreadable process entry",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, unreadable: true},
			selected: false,
		},
		{
			name:     "a process whose parent cannot be read",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, missingStat: true, workingDir: "/repo"},
			selected: false,
		},
		{
			name:     "a working directory that cannot be read, with roots present",
			process:  fakeProcess{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repo", "/live"}, parent: liveClient, noWorkingD: true},
			present:  []string{"/live"},
			selected: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanner, signalled := scannerOver([]fakeProcess{testCase.process}, 1, testCase.present...)
			results, err := scanner.Reap(false)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if !testCase.selected {
				if len(results) != 0 {
					t.Fatalf("expected nothing selected, got %+v", results)
				}
				if len(*signalled) != 0 {
					t.Fatalf("expected no signals, got %v", *signalled)
				}
				return
			}
			if len(results) != 1 {
				t.Fatalf("expected one selection, got %+v", results)
			}
			if results[0].Reason != testCase.reason {
				t.Fatalf("expected reason %q, got %q", testCase.reason, results[0].Reason)
			}
			if !slices.Equal(*signalled, []int{testCase.process.pid}) {
				t.Fatalf("expected pid %d signalled, got %v", testCase.process.pid, *signalled)
			}
		})
	}
}

// The scanner must never signal itself: a fresh server reaping at startup is an
// orphan candidate by every rule that matters here.
func TestReapNeverSignalsTheCallingProcess(t *testing.T) {
	scanner, signalled := scannerOver([]fakeProcess{
		{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repo", "/gone"}, parent: 1, workingDir: "/gone (deleted)"},
	}, 100)

	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 0 || len(*signalled) != 0 {
		t.Fatalf("the scanner selected itself: %+v %v", results, *signalled)
	}
}

func TestReapDryRunSignalsNothing(t *testing.T) {
	processes := []fakeProcess{
		{pid: 100, arguments: []string{"/bin/grafo", "mcp", "--repo", "/repo"}, parent: 1, workingDir: "/repo"},
		{pid: 200, arguments: []string{"/bin/grafo", "mcp", "--repo", "/repo"}, parent: liveClient, workingDir: "/repo"},
	}
	scanner, signalled := scannerOver(processes, 1, "/repo")

	dry, err := scanner.Reap(true)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(*signalled) != 0 {
		t.Fatalf("a dry run signalled %v", *signalled)
	}
	if len(dry) != 1 || dry[0].PID != 100 || dry[0].Signalled {
		t.Fatalf("expected pid 100 reported unsignalled, got %+v", dry)
	}

	wet, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(wet) != len(dry) || wet[0].PID != dry[0].PID || wet[0].Reason != dry[0].Reason {
		t.Fatalf("a dry run reported a different selection than the real one: %+v vs %+v", dry, wet)
	}
}

// Several orphans are reported in a stable order, so one run's report can be
// compared with the next.
func TestReapReportsSelectionsInAscendingProcessOrder(t *testing.T) {
	scanner, _ := scannerOver([]fakeProcess{
		{pid: 2000, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, workingDir: "/repo"},
		{pid: 30, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, workingDir: "/repo"},
		{pid: 400, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, workingDir: "/repo"},
	}, 1, "/repo")

	results, err := scanner.Reap(true)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	var order []int
	for _, result := range results {
		order = append(order, result.PID)
	}
	if !slices.Equal(order, []int{30, 400, 2000}) {
		t.Fatalf("expected ascending process order, got %v", order)
	}
}

// A process that exits between selection and the signal must not be reported as
// reaped, and its pid must not be signalled after the kernel could have reused
// it.
func TestReapSkipsAProcessThatExitedBeforeTheSignal(t *testing.T) {
	processes := []fakeProcess{
		{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, workingDir: "/repo"},
	}
	vanishing := &vanishingProcFS{ProcFS: fakeProcFS{processes: processes}}
	var signalled []int
	scanner := Scanner{
		Proc:   vanishing,
		Self:   1,
		Exists: func(string) bool { return true },
		Signal: func(pid int) error { signalled = append(signalled, pid); return nil },
	}

	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(signalled) != 0 {
		t.Fatalf("signalled a pid that had already exited: %v", signalled)
	}
	if len(results) != 1 || results[0].Signalled || results[0].Err == nil {
		t.Fatalf("expected the vanished process reported unsignalled with a reason, got %+v", results)
	}
}

// vanishingProcFS answers the first command-line read and then reports the
// process as gone, which is the pid-reuse window the second read closes.
type vanishingProcFS struct {
	ProcFS
	reads int
}

func (v *vanishingProcFS) ReadFile(name string) ([]byte, error) {
	if strings.HasSuffix(name, "/cmdline") {
		v.reads++
		if v.reads > 1 {
			return nil, fs.ErrNotExist
		}
	}
	return v.ProcFS.ReadFile(name)
}

// A platform without procfs reports that it scanned nothing rather than
// claiming there was nothing to find.
func TestReapWithoutProcFSReportsItIsUnsupported(t *testing.T) {
	_, err := Scanner{}.Reap(false)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// Non-numeric procfs entries are the kernel's own files, not processes.
func TestReapIgnoresNonProcessEntries(t *testing.T) {
	scanner := Scanner{
		Proc:   fakeProcFS{extra: []string{"self", "meminfo", "sys"}},
		Self:   1,
		Exists: func(string) bool { return true },
		Signal: func(int) error { return nil },
	}
	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no selections, got %+v", results)
	}
}

// A pid that is reused between selection and the signal must not be signalled,
// even when the process that inherited it looks identical. The command line
// alone cannot tell two servers apart, so identity includes the start time the
// kernel records, which a later process cannot share.
func TestReapSkipsAReusedProcessIdentifier(t *testing.T) {
	processes := []fakeProcess{
		{pid: 100, arguments: []string{"/bin/grafo", "mcp"}, parent: 1, workingDir: "/repo", startTime: 900},
	}
	restarting := &restartedProcFS{ProcFS: fakeProcFS{processes: processes}, restartAt: 1, startTime: 9000}
	var signalled []int
	scanner := Scanner{
		Proc:   restarting,
		Self:   1,
		Exists: func(string) bool { return true },
		Signal: func(pid int) error { signalled = append(signalled, pid); return nil },
	}

	results, err := scanner.Reap(false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(signalled) != 0 {
		t.Fatalf("signalled a reused pid: %v", signalled)
	}
	if len(results) != 1 || results[0].Signalled || results[0].Err == nil {
		t.Fatalf("expected the reused pid reported unsignalled with a reason, got %+v", results)
	}
}

// restartedProcFS answers the first status read with the selected process and
// every later one with a different start time, which is what a reused pid looks
// like.
type restartedProcFS struct {
	ProcFS
	reads     int
	restartAt int
	startTime int64
}

func (r *restartedProcFS) ReadFile(name string) ([]byte, error) {
	if strings.HasSuffix(name, "/stat") {
		r.reads++
		if r.reads > r.restartAt {
			return []byte("100 (grafo) S 1 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 18 0 " +
				strconv.FormatInt(r.startTime, 10) + " 0 0\n"), nil
		}
	}
	return r.ProcFS.ReadFile(name)
}

// A command name containing spaces and parentheses must not derail start-time
// parsing, which is why the field is read after the last closing parenthesis.
func TestProcessStartTimeSurvivesAnAwkwardCommandName(t *testing.T) {
	stat := []byte("100 ((my grafo) build) S 1 100 100 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 18 0 " +
		"4242 0 0\n")
	if got, ok := parseStartTime(stat); !ok || got != 4242 {
		t.Fatalf("parseStartTime = %d, %v, want 4242, true", got, ok)
	}
	if _, ok := parseStartTime([]byte("100 (grafo) S 1")); ok {
		t.Fatal("a truncated stat line was parsed as a start time")
	}
	if _, ok := parseStartTime([]byte("nothing useful")); ok {
		t.Fatal("a malformed stat line was parsed as a start time")
	}
}
