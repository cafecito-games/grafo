// Package profiling writes the runtime profiles one grafo run was asked for.
//
// Every profile is opt-in and absent by default, because each one costs
// something: the CPU and trace profiles install a sampler for the whole run, and
// the block and mutex profiles make the runtime record contention events it
// otherwise ignores. A run that asks for none pays nothing and takes no
// different path than before.
//
// The profiles are the standard ones `go tool pprof` and `go tool trace` read,
// at the rates the Go toolchain's own flags use, so a reader who knows
// `go test -cpuprofile` already knows what these contain.
package profiling

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
)

// Options names the file each profile is written to. An empty path leaves that
// profile off, which is what every run that did not ask for one passes.
type Options struct {
	CPUPath    string
	MemoryPath string
	BlockPath  string
	MutexPath  string
	TracePath  string
}

// Requested reports whether any profile was asked for, so a caller can skip the
// session entirely rather than start one that does nothing.
func (o Options) Requested() bool {
	return o.CPUPath != "" || o.MemoryPath != "" || o.BlockPath != "" ||
		o.MutexPath != "" || o.TracePath != ""
}

// blockProfileRate records every blocking event, which is what
// `go test -blockprofile` does. A sampled rate would hide exactly the question
// this profile is reached for — whether goroutines contend or merely queue —
// behind a threshold chosen without knowing the answer. It distorts a run's
// timings in exchange, which is why it is never on unless asked for.
const blockProfileRate = 1

// mutexProfileFraction reports every mutex contention event, matching
// `go test -mutexprofile` for the same reason as blockProfileRate.
const mutexProfileFraction = 1

// Session owns the files a started profiling run writes and the runtime state it
// changed. Stop returns both to where they were.
type Session struct {
	cpu    *os.File
	memory *os.File
	block  *os.File
	mutex  *os.File
	trace  *os.File
}

// Start opens every requested profile's file and installs the samplers that have
// to run for the whole command.
//
// Opening the files first is deliberate: a profile is asked for precisely when
// the run is expensive, and discovering an unwritable path after a cold index
// has finished wastes the only run the caller cared about. A failure here leaves
// nothing installed and no file open.
func Start(options Options) (*Session, error) {
	session := &Session{}
	for _, target := range []struct {
		path string
		file **os.File
	}{
		{options.CPUPath, &session.cpu},
		{options.MemoryPath, &session.memory},
		{options.BlockPath, &session.block},
		{options.MutexPath, &session.mutex},
		{options.TracePath, &session.trace},
	} {
		if target.path == "" {
			continue
		}
		file, err := os.Create(target.path)
		if err != nil {
			_ = session.closeFiles()
			return nil, fmt.Errorf("create profile %s: %w", target.path, err)
		}
		*target.file = file
	}
	if session.cpu != nil {
		// Only one CPU profile can run per process, so a second concurrent
		// request is reported rather than silently replacing the first.
		if err := pprof.StartCPUProfile(session.cpu); err != nil {
			_ = session.closeFiles()
			return nil, fmt.Errorf("start CPU profile: %w", err)
		}
	}
	if session.trace != nil {
		if err := trace.Start(session.trace); err != nil {
			if session.cpu != nil {
				pprof.StopCPUProfile()
			}
			_ = session.closeFiles()
			return nil, fmt.Errorf("start execution trace: %w", err)
		}
	}
	if session.block != nil {
		runtime.SetBlockProfileRate(blockProfileRate)
	}
	if session.mutex != nil {
		runtime.SetMutexProfileFraction(mutexProfileFraction)
	}
	return session, nil
}

// Stop writes the snapshot profiles, uninstalls the samplers, and closes every
// file. It attempts all of them and joins their errors, so one unwritable
// profile does not cost the others: a caller that asked for several has already
// paid for the run that produced them.
//
// A nil session stops nothing, which is what a run that requested no profile
// holds.
func (s *Session) Stop() error {
	if s == nil {
		return nil
	}
	var errs []error
	// The samplers stop first so the snapshots below do not record the profiler's
	// own shutdown.
	if s.trace != nil {
		trace.Stop()
	}
	if s.cpu != nil {
		pprof.StopCPUProfile()
	}
	if s.memory != nil {
		// The heap profile reports live objects, so it is only accurate after a
		// collection. Its cumulative allocation views do not need this, but they
		// are read from the same file.
		runtime.GC()
		errs = append(errs, writeProfile("heap", s.memory, s.memory.Name()))
	}
	if s.block != nil {
		errs = append(errs, writeProfile("block", s.block, s.block.Name()))
		runtime.SetBlockProfileRate(0)
	}
	if s.mutex != nil {
		errs = append(errs, writeProfile("mutex", s.mutex, s.mutex.Name()))
		runtime.SetMutexProfileFraction(0)
	}
	errs = append(errs, s.closeFiles())
	return errors.Join(errs...)
}

func writeProfile(name string, file *os.File, path string) error {
	profile := pprof.Lookup(name)
	if profile == nil {
		return fmt.Errorf("write profile %s: runtime has no %s profile", path, name)
	}
	if err := profile.WriteTo(file, 0); err != nil {
		return fmt.Errorf("write profile %s: %w", path, err)
	}
	return nil
}

// closeFiles closes every file the session opened, including after a partial
// Start, and reports what it could not close. A profile the caller is about to
// read is only complete once its file is closed, so a close failure is an error
// rather than something to drop.
func (s *Session) closeFiles() error {
	var errs []error
	for _, file := range []*os.File{s.cpu, s.memory, s.block, s.mutex, s.trace} {
		if file == nil {
			continue
		}
		if err := file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close profile %s: %w", file.Name(), err))
		}
	}
	return errors.Join(errs...)
}
