package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// A profile is reached for when a command is expensive, so the one command worth
// proving it against is a real index rather than a stub.
func TestIndexWritesEveryRequestedProfile(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module sample\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "charge.go"),
		[]byte("package sample\n\nfunc Charge() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := testtemp.Dir(t)
	path := func(name string) string { return filepath.Join(profiles, name) }
	code := run(t, "index", root,
		"--cpu-profile", path("cpu.pprof"),
		"--memory-profile", path("memory.pprof"),
		"--block-profile", path("block.pprof"),
		"--mutex-profile", path("mutex.pprof"),
		"--trace-profile", path("trace.out"))
	if code != 0 {
		t.Fatalf("index with profiling exited with %d", code)
	}
	for _, name := range []string{"cpu.pprof", "memory.pprof", "block.pprof", "mutex.pprof", "trace.out"} {
		info, err := os.Stat(path(name))
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

// An unwritable profile path has to be refused before the command runs, because
// the run it was asked to measure is the expensive part.
func TestProfilingRefusesAnUnwritablePathBeforeTheCommandRuns(t *testing.T) {
	root := testtemp.Dir(t)
	blocker := filepath.Join(testtemp.Dir(t), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := output(t, "index", root, "--cpu-profile", filepath.Join(blocker, "cpu.pprof"))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".grafo")); err == nil {
		t.Fatal("the index was written despite the refused profile path")
	}
}

// Profiling is universal, so no command may reject the options the way the
// per-command ones are rejected.
func TestProfilingOptionsAreAcceptedByEveryCommand(t *testing.T) {
	for _, name := range []string{"cpu-profile", "memory-profile", "block-profile", "mutex-profile", "trace-profile"} {
		t.Run(name, func(t *testing.T) {
			args, err := parseArguments([]string{"find", "Charge", "--" + name, "out"})
			if err != nil {
				t.Fatalf("parseArguments rejected --%s: %v", name, err)
			}
			if args.values[name] != "out" {
				t.Fatalf("--%s = %q, want %q", name, args.values[name], "out")
			}
		})
	}
}

func TestProfilingOptionsReadEveryPath(t *testing.T) {
	args, err := parseArguments([]string{"index",
		"--cpu-profile", "c", "--memory-profile", "m", "--block-profile", "b",
		"--mutex-profile", "x", "--trace-profile", "t"})
	if err != nil {
		t.Fatal(err)
	}
	options := profilingOptions(args)
	if options.CPUPath != "c" || options.MemoryPath != "m" || options.BlockPath != "b" ||
		options.MutexPath != "x" || options.TracePath != "t" {
		t.Fatalf("profilingOptions = %#v", options)
	}
	if !options.Requested() {
		t.Fatal("Requested() = false with every path set")
	}
	if profilingOptions(parsedArguments{values: map[string]string{}}).Requested() {
		t.Fatal("Requested() = true with no paths set")
	}
}
