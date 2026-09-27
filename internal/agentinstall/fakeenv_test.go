package agentinstall

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type invocation struct {
	name      string
	arguments []string
}

type writeRecord struct {
	path string
	data string
}

// fakeEnvironment is an in-memory Environment. Every mutating operation is
// recorded, and strict mode turns any mutation into a recorded violation so a
// test can prove that detection and dry-run wrote nothing.
type fakeEnvironment struct {
	goos    string
	home    string
	temp    string
	vars    map[string]string
	files   map[string]string
	dirs    map[string]bool
	lookups map[string]string
	outputs map[string]string
	runErrs map[string]error

	onRun       func(name string, arguments []string) ([]byte, error)
	invocations []invocation
	writes      []writeRecord
	mkdirs      []string
	violations  []string
	strict      bool
}

func newFakeEnvironment(goos, home string) *fakeEnvironment {
	temp := "/tmp"
	if goos == "windows" {
		temp = `C:\Users\u\AppData\Local\Temp`
	}
	return &fakeEnvironment{
		goos:    goos,
		home:    home,
		temp:    temp,
		vars:    map[string]string{},
		files:   map[string]string{},
		dirs:    map[string]bool{},
		lookups: map[string]string{},
		outputs: map[string]string{},
		runErrs: map[string]error{},
	}
}

// withFixture seeds path with the contents of a testdata fixture.
func (f *fakeEnvironment) withFixture(t *testing.T, path, fixture string) *fakeEnvironment {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	f.files[path] = string(data)
	return f
}

func (f *fakeEnvironment) LookPath(file string) (string, error) {
	if path := f.lookups[file]; path != "" {
		return path, nil
	}
	return "", errors.New("executable file not found in $PATH")
}

func (f *fakeEnvironment) Output(_ context.Context, name string, arguments ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, arguments...), " ")
	output, ok := f.outputs[key]
	if !ok {
		return nil, errors.New("command unavailable")
	}
	return []byte(output), nil
}

func (f *fakeEnvironment) ReadFile(name string) ([]byte, error) {
	contents, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(contents), nil
}

type fakeInfo struct {
	name  string
	size  int64
	isDir bool
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.isDir }
func (i fakeInfo) Sys() any           { return nil }

func (f *fakeEnvironment) Stat(name string) (fs.FileInfo, error) {
	if contents, ok := f.files[name]; ok {
		return fakeInfo{name: name, size: int64(len(contents))}, nil
	}
	if f.dirs[name] {
		return fakeInfo{name: name, isDir: true}, nil
	}
	prefix := name + pathSeparator(f.goos)
	for path := range f.files {
		if strings.HasPrefix(path, prefix) {
			return fakeInfo{name: name, isDir: true}, nil
		}
	}
	for path := range f.dirs {
		if strings.HasPrefix(path, prefix) {
			return fakeInfo{name: name, isDir: true}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *fakeEnvironment) GOOS() string { return f.goos }

func (f *fakeEnvironment) HomeDir() (string, error) {
	if f.home == "" {
		return "", errors.New("home directory unavailable")
	}
	return f.home, nil
}

func (f *fakeEnvironment) Getenv(key string) string { return f.vars[key] }

func (f *fakeEnvironment) TempDir() string { return f.temp }

func (f *fakeEnvironment) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	if f.strict {
		f.violations = append(f.violations, "Run "+name)
		return nil, errors.New("unexpected mutation")
	}
	f.invocations = append(f.invocations, invocation{name: name, arguments: arguments})
	if f.onRun != nil {
		return f.onRun(name, arguments)
	}
	if err := f.runErrs[name]; err != nil {
		return []byte("client command failed"), err
	}
	return nil, nil
}

func (f *fakeEnvironment) MkdirAll(path string, _ fs.FileMode) error {
	if f.strict {
		f.violations = append(f.violations, "MkdirAll "+path)
		return errors.New("unexpected mutation")
	}
	f.mkdirs = append(f.mkdirs, path)
	f.dirs[path] = true
	return nil
}

func (f *fakeEnvironment) WriteFileAtomic(path string, data []byte, _ fs.FileMode) error {
	if f.strict {
		f.violations = append(f.violations, "WriteFileAtomic "+path)
		return errors.New("unexpected mutation")
	}
	f.writes = append(f.writes, writeRecord{path: path, data: string(data)})
	f.files[path] = string(data)
	return nil
}

var _ Environment = (*fakeEnvironment)(nil)

func (f *fakeEnvironment) assertNoMutations(t *testing.T) {
	t.Helper()
	if len(f.violations) > 0 {
		t.Fatalf("unexpected mutations: %v", f.violations)
	}
	if len(f.writes) > 0 || len(f.mkdirs) > 0 || len(f.invocations) > 0 {
		t.Fatalf("unexpected mutations: writes=%v mkdirs=%v invocations=%v", f.writes, f.mkdirs, f.invocations)
	}
}
