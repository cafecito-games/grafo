package service

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestLockReportsContentionAsErrLockBusy pins the sentinel callers match on: a
// wait that elapses while a competitor holds the lock is ordinary contention,
// and has to stay distinguishable from a lock that could not be taken at all,
// so the CLI can render guidance instead of a lock-file path.
func TestLockReportsContentionAsErrLockBusy(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "index.db.lock")
	unlock, err := Lock(path, time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer func() { _ = unlock() }()
	_, err = Lock(path, 10*time.Millisecond)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("contended Lock error = %v, want it to wrap ErrLockBusy", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("contended Lock error = %q, want it to name %q", err, path)
	}
}
