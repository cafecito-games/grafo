package profiling

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// Nothing in this package runs in parallel. Every profile is process-global
// runtime state, so two sessions overlapping would make each other's
// assertions depend on test order rather than on behavior.

func TestOptionsReportWhetherAnyProfileWasRequested(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options Options
		want    bool
	}{
		{name: "none", options: Options{}, want: false},
		{name: "cpu", options: Options{CPUPath: "cpu.pprof"}, want: true},
		{name: "memory", options: Options{MemoryPath: "memory.pprof"}, want: true},
		{name: "block", options: Options{BlockPath: "block.pprof"}, want: true},
		{name: "mutex", options: Options{MutexPath: "mutex.pprof"}, want: true},
		{name: "trace", options: Options{TracePath: "trace.out"}, want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.options.Requested(); got != testCase.want {
				t.Fatalf("Requested() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestStartWritesEveryRequestedProfile(t *testing.T) {
	directory := testtemp.Dir(t)
	options := Options{
		CPUPath:    filepath.Join(directory, "cpu.pprof"),
		MemoryPath: filepath.Join(directory, "memory.pprof"),
		BlockPath:  filepath.Join(directory, "block.pprof"),
		MutexPath:  filepath.Join(directory, "mutex.pprof"),
		TracePath:  filepath.Join(directory, "trace.out"),
	}
	session, err := Start(options)
	if err != nil {
		t.Fatal(err)
	}
	contendOnALock()
	if err := session.Stop(); err != nil {
		t.Fatal(err)
	}

	// The pprof profiles are gzipped protocol buffers; a trace is its own
	// uncompressed format, so only its presence is asserted.
	for _, path := range []string{options.CPUPath, options.MemoryPath, options.BlockPath, options.MutexPath} {
		assertGzippedProfile(t, path)
	}
	assertNonEmpty(t, options.TracePath)
}

func TestStartLeavesNothingInstalledWhenAProfileCannotBeCreated(t *testing.T) {
	directory := testtemp.Dir(t)
	// A path under a regular file can never be created, on any platform.
	blocker := filepath.Join(directory, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cpuPath := filepath.Join(directory, "cpu.pprof")
	if _, err := Start(Options{CPUPath: cpuPath, MemoryPath: filepath.Join(blocker, "memory.pprof")}); err == nil {
		t.Fatal("Start succeeded with an uncreatable memory profile path")
	}

	// The CPU profiler must not have been left running, which a second Start
	// proves: pprof refuses a concurrent one.
	session, err := Start(Options{CPUPath: cpuPath})
	if err != nil {
		t.Fatalf("CPU profiler was left running by the failed Start: %v", err)
	}
	if err := session.Stop(); err != nil {
		t.Fatal(err)
	}
	assertGzippedProfile(t, cpuPath)
}

func TestStartRefusesASecondConcurrentCPUProfile(t *testing.T) {
	directory := testtemp.Dir(t)
	first, err := Start(Options{CPUPath: filepath.Join(directory, "first.pprof")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Start(Options{CPUPath: filepath.Join(directory, "second.pprof")})
	if err == nil {
		_ = second.Stop()
		_ = first.Stop()
		t.Fatal("Start accepted a second concurrent CPU profile")
	}
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
}

// A run that asked for no profile holds no session, and stopping it has to be
// safe so the caller needs no special case for the ordinary run.
func TestStopIsSafeOnANilSessionAndOnAnEmptyOne(t *testing.T) {
	var absent *Session
	if err := absent.Stop(); err != nil {
		t.Fatalf("nil session Stop() = %v", err)
	}
	session, err := Start(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Stop(); err != nil {
		t.Fatalf("empty session Stop() = %v", err)
	}
}

// Stop has to uninstall the contention samplers, or every later command in the
// same process keeps paying for a profile nobody asked for. A second session
// that requests no block profile must therefore record no new blocking events.
func TestStopUninstallsTheContentionSamplers(t *testing.T) {
	directory := testtemp.Dir(t)
	session, err := Start(Options{BlockPath: filepath.Join(directory, "block.pprof")})
	if err != nil {
		t.Fatal(err)
	}
	contendOnALock()
	if err := session.Stop(); err != nil {
		t.Fatal(err)
	}
	before := pprof.Lookup("block").Count()
	contendOnALock()
	if after := pprof.Lookup("block").Count(); after != before {
		t.Fatalf("block profile grew from %d to %d after Stop; the sampler is still installed", before, after)
	}
}

// contendOnALock produces the blocking and mutex contention the profiles are
// asserted to have captured.
func contendOnALock() {
	var lock sync.Mutex
	var group sync.WaitGroup
	done := make(chan struct{})
	lock.Lock()
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			lock.Lock()
			defer lock.Unlock()
			<-done
		}()
	}
	close(done)
	lock.Unlock()
	group.Wait()
}

func assertGzippedProfile(t *testing.T, path string) {
	t.Helper()
	contents := assertNonEmpty(t, path)
	reader, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		t.Fatalf("%s is not a gzipped pprof profile: %v", path, err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("%s could not be decompressed: %v", path, err)
	}
	if len(decoded) == 0 {
		t.Fatalf("%s decompressed to nothing", path)
	}
}

func assertNonEmpty(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return contents
}
