package mcpreap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ErrUnsupported reports that this platform offers no way to enumerate
// processes, so a caller never reads an empty result as proof that nothing was
// left behind.
var ErrUnsupported = errors.New("process scanning is unavailable on this platform")

// errVanished reports a process that exited between selection and the signal.
var errVanished = errors.New("exited before it could be signalled")

// Reason names what proved a server has no client left.
type Reason string

const (
	// ReasonClientExited is a server reparented to init. A second process
	// cannot know another's original parent, so the scanner uses the one parent
	// id that is conclusive on its own rather than the watchdog's "changed".
	ReasonClientExited Reason = "client exited"
	// ReasonRootsRemoved is a server every one of whose repository roots is
	// gone from disk.
	ReasonRootsRemoved Reason = "repository roots removed"
	// ReasonWorkingDirectoryDeleted is a server whose working directory was
	// unlinked, which is what a removed worktree leaves behind.
	ReasonWorkingDirectoryDeleted Reason = "working directory deleted"
)

// Result is one selected server and what became of it.
type Result struct {
	PID       int
	Arguments []string
	Reason    Reason
	Signalled bool
	Err       error
}

// ProcFS is the three procfs reads the scanner needs, as an interface so
// selection is tested against fabricated process trees instead of whatever
// happens to be running on the machine under test.
type ProcFS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	ReadLink(name string) (string, error)
}

// Scanner finds stdio MCP servers that outlived their clients and asks them to
// stop. Every effect is injected: the process table it reads, the existence
// check it trusts, and the signal it sends.
type Scanner struct {
	// Proc is the mounted procfs. A nil Proc means this platform has none.
	Proc ProcFS
	// Self is the calling process, which is never a candidate.
	Self int
	// Exists reports whether a repository root is still present. Only an
	// answer that it is not counts as removal; see rootPresent.
	Exists func(string) bool
	// Signal asks one process to shut down.
	Signal func(pid int) error
}

// NewScanner returns the scanner for this machine. On a platform without procfs
// it reports ErrUnsupported from Reap rather than silently finding nothing.
func NewScanner() Scanner {
	scanner := Scanner{
		Self:   os.Getpid(),
		Exists: rootPresent,
		// SIGTERM and nothing more. A server that ignores it is a separate bug,
		// and escalating to SIGKILL would make this the one part of the cleanup
		// capable of interrupting a write.
		Signal: func(pid int) error {
			process, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			return process.Signal(syscall.SIGTERM)
		},
	}
	if runtime.GOOS == "linux" {
		scanner.Proc = osProcFS{root: "/proc"}
	}
	return scanner
}

// Reap signals every grafo MCP server that has no client left, in ascending
// process order so one run's report can be compared with the next. A dry run
// reports the same selection and signals nothing.
//
// A process the scanner cannot read is skipped rather than guessed at: a denied
// read means another user's process, and a vanished entry means it has already
// exited. Both are cases where this cleanup has nothing to prove.
func (s Scanner) Reap(dryRun bool) ([]Result, error) {
	if s.Proc == nil {
		return nil, ErrUnsupported
	}
	entries, err := s.Proc.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("reading the process table: %w", err)
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, convertErr := strconv.Atoi(entry.Name())
		if convertErr != nil || pid <= 0 || pid == s.Self {
			continue
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	var results []Result
	for _, pid := range pids {
		arguments, ok := s.serverArguments(pid)
		if !ok {
			continue
		}
		reason, selected := s.reasonFor(pid, arguments)
		if !selected {
			continue
		}
		result := Result{PID: pid, Arguments: arguments, Reason: reason}
		if !dryRun {
			started, _ := s.startTimeOf(pid)
			result.Signalled, result.Err = s.signal(pid, arguments, started)
		}
		results = append(results, result)
	}
	return results, nil
}

// serverArguments reports the command line of a grafo MCP server, or false for
// any other process.
//
// The shape matched is the one every client is configured with, `grafo mcp`
// followed by its options: that is what grafo writes into a client's server
// registration. An invocation whose first argument is a flag instead is not
// claimed, which errs toward leaving a process alone.
func (s Scanner) serverArguments(pid int) ([]string, bool) {
	raw, err := s.Proc.ReadFile(strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, false
	}
	arguments := splitCommandLine(raw)
	if len(arguments) < 2 || arguments[1] != "mcp" {
		return nil, false
	}
	switch filepath.Base(arguments[0]) {
	case "grafo", "grafo.exe":
		return arguments, true
	}
	return nil, false
}

// splitCommandLine decodes a NUL-separated procfs command line. A command line
// without separators cannot be split into arguments and yields none.
func splitCommandLine(raw []byte) []string {
	fields := strings.Split(string(raw), "\x00")
	arguments := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			arguments = append(arguments, field)
		}
	}
	if len(arguments) == 1 && strings.Contains(arguments[0], " ") {
		return nil
	}
	return arguments
}

// reasonFor reports what proves this server has no client left.
func (s Scanner) reasonFor(pid int, arguments []string) (Reason, bool) {
	parent, ok := s.parentOf(pid)
	if !ok {
		return "", false
	}
	if parent == 1 {
		return ReasonClientExited, true
	}
	workingDirectory, deleted, known := s.workingDirectoryOf(pid)
	if roots := rootsFrom(arguments, workingDirectory); len(roots) > 0 {
		gone := true
		for _, root := range roots {
			if s.Exists(root) {
				gone = false
				break
			}
		}
		if gone {
			return ReasonRootsRemoved, true
		}
	}
	if known && deleted {
		return ReasonWorkingDirectoryDeleted, true
	}
	return "", false
}

// parentOf reads one process's parent from its status file.
func (s Scanner) parentOf(pid int) (int, bool) {
	raw, err := s.Proc.ReadFile(strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(raw)) {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "PPid:")
		if !found {
			continue
		}
		parent, convertErr := strconv.Atoi(strings.TrimSpace(value))
		if convertErr != nil {
			return 0, false
		}
		return parent, true
	}
	return 0, false
}

// startTimeOf reports when the kernel started one process, in clock ticks since
// boot, which together with the pid identifies a process across its lifetime.
func (s Scanner) startTimeOf(pid int) (int64, bool) {
	raw, err := s.Proc.ReadFile(strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseStartTime(raw)
}

// startTimeField is the one-based position of starttime in a procfs stat line,
// counting the fields that follow the command name.
const startTimeField = 20

// parseStartTime reads the start time out of a procfs stat line.
//
// The fields are read after the last closing parenthesis, because the command
// name is the second field, is parenthesized, and may itself contain spaces and
// parentheses: splitting the whole line on spaces would shift every field after
// a name like "(my grafo) build".
func parseStartTime(raw []byte) (int64, bool) {
	line := string(raw)
	end := strings.LastIndex(line, ")")
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(line[end+1:])
	if len(fields) < startTimeField {
		return 0, false
	}
	started, err := strconv.ParseInt(fields[startTimeField-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return started, true
}

// deletedSuffix is what the kernel appends to a working directory link whose
// target has been unlinked.
const deletedSuffix = " (deleted)"

// workingDirectoryOf reports a process's working directory, whether it has been
// deleted, and whether either could be read at all.
func (s Scanner) workingDirectoryOf(pid int) (directory string, deleted bool, known bool) {
	target, err := s.Proc.ReadLink(strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return "", false, false
	}
	if trimmed, found := strings.CutSuffix(target, deletedSuffix); found {
		return trimmed, true, true
	}
	return target, false, true
}

// rootsFrom recovers the repository roots a server was invoked with, resolving
// a relative root against the server's own working directory. A server invoked
// without roots reports none: its root is its working directory, which the
// deleted-directory rule already covers.
func rootsFrom(arguments []string, workingDirectory string) []string {
	var roots []string
	for index := 1; index < len(arguments); index++ {
		option, isOption := strings.CutPrefix(arguments[index], "--")
		if !isOption {
			continue
		}
		name, value, hasValue := strings.Cut(option, "=")
		if name != "repo" && name != "repos" {
			continue
		}
		if !hasValue {
			index++
			if index >= len(arguments) {
				break
			}
			value = arguments[index]
		}
		for _, root := range strings.Split(value, ",") {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			if !filepath.IsAbs(root) {
				if workingDirectory == "" {
					continue
				}
				root = filepath.Join(workingDirectory, root)
			}
			roots = append(roots, root)
		}
	}
	return roots
}

// signal asks one selected server to stop, after confirming it is still the
// process that was selected.
//
// A pid identifies a process only while that process lives. One that exits
// between selection and the signal frees its number for something else, and a
// command line cannot tell two servers apart, so identity is the command line
// together with the start time the kernel recorded: a later process occupying
// the same number cannot share it. Without that check the cleanup could stop a
// live server that merely inherited the number of a dead one.
func (s Scanner) signal(pid int, arguments []string, started int64) (bool, error) {
	current, ok := s.serverArguments(pid)
	if !ok || !slices.Equal(current, arguments) {
		return false, errVanished
	}
	if now, known := s.startTimeOf(pid); !known || now != started {
		return false, errVanished
	}
	if err := s.Signal(pid); err != nil {
		return false, err
	}
	return true, nil
}

// osProcFS is the real procfs. ReadLink is its own method because io/fs offers
// no way to read a symbolic link through a plain fs.FS.
type osProcFS struct{ root string }

func (o osProcFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(o.join(name))
}

func (o osProcFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(o.join(name))
}

func (o osProcFS) ReadLink(name string) (string, error) {
	return os.Readlink(o.join(name))
}

func (o osProcFS) join(name string) string {
	if name == "." {
		return o.root
	}
	return filepath.Join(o.root, path.Clean(name))
}
