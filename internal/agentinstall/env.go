package agentinstall

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Reader is the read-only view of the outside world. Detection and dry-run
// planning are restricted to this interface so that neither can mutate state:
// there is simply no write operation reachable from here.
type Reader interface {
	// LookPath resolves an executable on PATH.
	LookPath(file string) (string, error)
	// Output runs a read-only informational command (`--help`, `mcp list`).
	Output(ctx context.Context, name string, arguments ...string) ([]byte, error)
	// ReadFile reads a configuration file.
	ReadFile(name string) ([]byte, error)
	// Stat reports whether a path exists, following symlinks.
	Stat(name string) (fs.FileInfo, error)
	// Lstat reports on a path without following symlinks, so the installer can
	// refuse to write through one.
	Lstat(name string) (fs.FileInfo, error)
	// GOOS reports the target operating system ("linux", "darwin", "windows").
	GOOS() string
	// HomeDir reports the current user's home directory.
	HomeDir() (string, error)
	// Getenv reads an environment variable.
	Getenv(key string) string
	// TempDir reports the directory used for temporary files.
	TempDir() string
}

// Writer holds the mutating operations. Only Install and Uninstall outside of
// dry-run mode ever receive a value that satisfies it.
type Writer interface {
	// Run executes a client command that changes configuration.
	Run(ctx context.Context, name string, arguments ...string) ([]byte, error)
	// MkdirAll creates a configuration directory and its parents.
	MkdirAll(path string, perm fs.FileMode) error
	// WriteFileAtomic replaces path's contents by writing a temporary file in
	// the same directory and renaming it over path.
	WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
	// Remove deletes one file or empty directory Grafo owns.
	Remove(path string) error
}

// Environment bundles every outside-world operation the installer needs.
type Environment interface {
	Reader
	Writer
}

// Runner provides the process operations needed to configure a client. It is
// retained for compatibility with callers written against the original
// process-only seam; Environment embeds the same three operations.
type Runner interface {
	LookPath(file string) (string, error)
	Output(ctx context.Context, name string, arguments ...string) ([]byte, error)
	Run(ctx context.Context, name string, arguments ...string) ([]byte, error)
}

// ExecRunner configures clients through their installed command-line tools.
type ExecRunner struct{}

func (ExecRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (ExecRunner) Output(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).Output()
}

func (ExecRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
}

// OSEnvironment is the production Environment: real processes, real files.
type OSEnvironment struct {
	ExecRunner
}

// NewOSEnvironment returns the Environment backed by the host operating system.
func NewOSEnvironment() OSEnvironment { return OSEnvironment{} }

func (OSEnvironment) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (OSEnvironment) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

func (OSEnvironment) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func (OSEnvironment) Remove(path string) error { return os.Remove(path) }

func (OSEnvironment) GOOS() string { return runtime.GOOS }

func (OSEnvironment) HomeDir() (string, error) { return os.UserHomeDir() }

func (OSEnvironment) Getenv(key string) string { return os.Getenv(key) }

func (OSEnvironment) TempDir() string { return os.TempDir() }

func (OSEnvironment) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

// WriteFileAtomic writes to a temporary file in path's directory and renames it
// over path, so a failure leaves the original configuration untouched.
func (OSEnvironment) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".grafo-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}()
	if _, err = temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Chmod(name, perm); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	name = ""
	return nil
}

var _ Environment = OSEnvironment{}

// resolveExecutable canonicalizes the Grafo binary path and refuses paths that
// are empty or inside the temporary directory, which would not survive.
func resolveExecutable(reader Reader, executable string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		return "", fmt.Errorf("locate grafo executable: empty path")
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("locate grafo executable: %w", err)
	}
	if temporary := strings.TrimSpace(reader.TempDir()); temporary != "" {
		cleaned := filepath.Clean(temporary)
		if absolute == cleaned || strings.HasPrefix(absolute, cleaned+string(filepath.Separator)) {
			return "", fmt.Errorf("refusing to register temporary grafo executable %q", absolute)
		}
	}
	return absolute, nil
}
