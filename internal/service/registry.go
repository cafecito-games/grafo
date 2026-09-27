// Package service runs Grafo's local background indexing supervisor, owns the
// user-level registry of watched repository roots, generates the platform
// service definitions that start the supervisor, and reports diagnostics for all
// of it.
//
// Three authorities are consumed rather than duplicated: the registry owns which
// roots are watched, indexer.DiscoverProject owns branch and index identity, and
// internal/indexer owns indexing semantics. Ownership of anything Grafo writes
// outside a repository is proven through the agentinstall receipt ledger and
// bounded by agentinstall.CheckUserConfigRoot.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/agentinstall"
)

// registryFormat is the on-disk format marker, checked exactly so that a future
// format is reported instead of being overwritten.
const registryFormat = "grafo.service.registry/1"

// DefaultInterval is the reconciliation interval used when a root does not pin
// one of its own.
const DefaultInterval = 5 * time.Second

// MinimumInterval bounds how often a root may be reconciled.
const MinimumInterval = 500 * time.Millisecond

// now is the clock, replaced in tests so registry documents are comparable.
var now = func() time.Time { return time.Now().UTC() }

// Settings are the per-root options the user may pin when registering a root.
type Settings struct {
	// Interval is the reconciliation interval for this root. Zero means
	// DefaultInterval.
	Interval time.Duration
	// Paused registers the root without indexing it.
	Paused bool
}

// Root is one registered repository root.
type Root struct {
	Root     string `json:"root"`
	Name     string `json:"name"`
	Interval string `json:"interval,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
	AddedAt  string `json:"added_at,omitempty"`
}

// Every returns the reconciliation interval of a root, falling back to the
// default when the stored value is absent or out of range.
func (r Root) Every() time.Duration {
	if r.Interval == "" {
		return DefaultInterval
	}
	parsed, err := time.ParseDuration(r.Interval)
	if err != nil || parsed < MinimumInterval {
		return DefaultInterval
	}
	return parsed
}

// Registry is the user-level document listing every watched root.
type Registry struct {
	Format string `json:"format"`
	Roots  []Root `json:"roots"`
}

// Lookup returns the registered entry for a canonical root path.
func (r Registry) Lookup(root string) (Root, bool) {
	index := slices.IndexFunc(r.Roots, func(entry Root) bool { return entry.Root == root })
	if index == -1 {
		return Root{}, false
	}
	return r.Roots[index], true
}

// Store reads and writes the registry. Reads never mutate anything; writes take
// an exclusive cross-process lock, replace the file atomically, and preserve
// mode 0600 where the platform supports it.
type Store struct {
	env agentinstall.Environment
	// lock is the cross-process lock factory, replaced in tests that must
	// observe a locked registry.
	lock func(path string, wait time.Duration) (Unlock, error)
	// wait bounds how long a mutation waits for the lock.
	wait time.Duration
}

// NewStore returns the registry store backed by env.
func NewStore(env agentinstall.Environment) *Store {
	return &Store{env: env, lock: Lock, wait: 5 * time.Second}
}

// StateDir returns the Grafo configuration directory that holds the registry,
// the supervisor status file and the service logs.
func StateDir(reader agentinstall.Reader) (string, error) {
	return agentinstall.ConfigHomePath(reader, "grafo")
}

// Path returns the registry file location.
func (s *Store) Path() (string, error) {
	return agentinstall.ConfigHomePath(s.env, "grafo", "watched-roots.json")
}

// Load reads the registry. A missing file yields an empty registry; a malformed
// file or an unknown format is an error that reports how to recover, and the
// file is left byte-identical.
func (s *Store) Load() (Registry, error) {
	path, err := s.Path()
	if err != nil {
		return Registry{}, err
	}
	return s.load(path)
}

func (s *Store) load(path string) (Registry, error) {
	data, err := s.env.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Registry{Format: registryFormat}, nil
		}
		return Registry{}, fmt.Errorf("read grafo watched-root registry %s: %w; fix its permissions to continue", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return Registry{Format: registryFormat}, nil
	}
	var document Registry
	if err := json.Unmarshal(data, &document); err != nil {
		return Registry{}, fmt.Errorf("grafo watched-root registry %s is malformed: %w; move it aside to continue", path, err)
	}
	if document.Format != registryFormat {
		return Registry{}, fmt.Errorf("grafo watched-root registry %s uses unsupported format %q (expected %q); move it aside to continue",
			path, document.Format, registryFormat)
	}
	for index, entry := range document.Roots {
		if strings.TrimSpace(entry.Root) == "" {
			return Registry{}, fmt.Errorf("grafo watched-root registry %s entry %d has an empty root; move it aside to continue", path, index)
		}
	}
	slices.SortStableFunc(document.Roots, func(left, right Root) int {
		return strings.Compare(left.Root, right.Root)
	})
	return document, nil
}

// Add registers a root. The path is canonicalized first, so the same repository
// reached through a symlink or a relative path registers exactly once.
func (s *Store) Add(root string, settings Settings) (Registry, bool, error) {
	canonical, name, err := CanonicalRoot(root)
	if err != nil {
		return Registry{}, false, err
	}
	interval := ""
	if settings.Interval > 0 {
		if settings.Interval < MinimumInterval {
			return Registry{}, false, fmt.Errorf("interval must be at least %s", MinimumInterval)
		}
		interval = settings.Interval.String()
	}
	var changed bool
	document, err := s.mutate(func(current *Registry) error {
		entry := Root{Root: canonical, Name: name, Interval: interval, Paused: settings.Paused, AddedAt: now().Format(time.RFC3339)}
		index := slices.IndexFunc(current.Roots, func(existing Root) bool { return existing.Root == canonical })
		if index >= 0 {
			entry.AddedAt = current.Roots[index].AddedAt
			if current.Roots[index] == entry {
				return nil
			}
			current.Roots[index] = entry
			changed = true
			return nil
		}
		current.Roots = append(current.Roots, entry)
		changed = true
		return nil
	})
	return document, changed, err
}

// Remove unregisters a root. Removing an unregistered root is not an error, so
// the operation is safely replayable.
func (s *Store) Remove(root string) (Registry, bool, error) {
	canonical, _, err := CanonicalRoot(root)
	if err != nil {
		// A deleted root can no longer be canonicalized, so fall back to the
		// lexical absolute path: the registry must stay editable after the
		// repository is gone.
		absolute, absErr := filepath.Abs(root)
		if absErr != nil {
			return Registry{}, false, err
		}
		canonical = filepath.Clean(absolute)
	}
	var changed bool
	document, err := s.mutate(func(current *Registry) error {
		filtered := slices.DeleteFunc(slices.Clone(current.Roots), func(entry Root) bool {
			return entry.Root == canonical
		})
		if len(filtered) != len(current.Roots) {
			current.Roots = filtered
			changed = true
		}
		return nil
	})
	return document, changed, err
}

// mutate applies change under the registry lock and writes the result
// atomically. A malformed registry blocks every mutation, so no daemon state is
// derived from a document Grafo cannot read.
func (s *Store) mutate(change func(*Registry) error) (Registry, error) {
	path, err := s.Path()
	if err != nil {
		return Registry{}, err
	}
	if err := agentinstall.CheckUserConfigRoot(s.env, path); err != nil {
		return Registry{}, fmt.Errorf("refusing to write registry %s: %w", path, err)
	}
	if directory := agentinstall.ParentPath(s.env.GOOS(), path); directory != "" {
		if err := s.env.MkdirAll(directory, 0o700); err != nil {
			return Registry{}, fmt.Errorf("create grafo configuration directory %s: %w", directory, err)
		}
	}
	unlock, err := s.lock(path+".lock", s.wait)
	if err != nil {
		return Registry{}, err
	}
	defer func() { _ = unlock() }()
	document, err := s.load(path)
	if err != nil {
		return Registry{}, err
	}
	before := document
	if err := change(&document); err != nil {
		return Registry{}, err
	}
	document.Format = registryFormat
	slices.SortStableFunc(document.Roots, func(left, right Root) int {
		return strings.Compare(left.Root, right.Root)
	})
	if document.Roots == nil {
		document.Roots = []Root{}
	}
	if slices.Equal(before.Roots, document.Roots) && before.Format == document.Format {
		return document, nil
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return Registry{}, err
	}
	data = append(data, '\n')
	if err := s.env.WriteFileAtomic(path, data, 0o600); err != nil {
		return Registry{}, fmt.Errorf("write grafo watched-root registry %s: %w", path, err)
	}
	return document, nil
}

// CanonicalRoot resolves a user-supplied repository path to an absolute,
// symlink-resolved directory together with its display name, so the same
// directory registers exactly once however it is spelled. Branch and index
// identity are not decided here: indexer.DiscoverProject remains the authority
// for those, and it resolves the git top level from this path at run time.
func CanonicalRoot(root string) (string, string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", "", fmt.Errorf("read repository root %s: %w", absolute, err)
	}
	if !info.IsDir() {
		absolute = filepath.Dir(absolute)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root %s: %w", absolute, err)
	}
	resolved = filepath.Clean(resolved)
	return resolved, filepath.Base(resolved), nil
}
