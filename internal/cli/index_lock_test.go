package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/service"
)

// shortIndexLockWait keeps contention observable without waiting out the
// production budget. It replaces a package-level variable every index path
// reads, so a test that calls it cannot also call t.Parallel: the substitution
// would decide the lock budget of whatever else was running.
func shortIndexLockWait(t *testing.T, wait time.Duration) {
	t.Helper()
	previous := foregroundIndexLockWait
	foregroundIndexLockWait = wait
	t.Cleanup(func() { foregroundIndexLockWait = previous })
}

// TestOpenExistingWaitsForTheIndexLock pins the invariant IndexLock documents.
// Every query command refreshes the index first, which is a write, so it has
// to serialize behind `grafo index`, `grafo watch` and the supervisor instead
// of racing them as a second writer.
func TestOpenExistingWaitsForTheIndexLock(t *testing.T) {
	root := indexedRepository(t)
	ctx := context.Background()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, acquired, err := service.TryIndexLock(project.IndexPath)
	if err != nil || !acquired {
		t.Fatalf("TryIndexLock acquired=%v err=%v", acquired, err)
	}
	shortIndexLockWait(t, 50*time.Millisecond)

	_, repository, release, err := openExisting(ctx, root)
	if err == nil {
		_ = repository.Close()
		_ = release()
		_ = unlock()
		t.Fatal("openExisting refreshed the index while another process held the index lock")
	}
	if !strings.Contains(err.Error(), "another grafo process is refreshing this branch index") {
		t.Fatalf("contended openExisting error = %q, want actionable guidance", err)
	}

	if err := unlock(); err != nil {
		t.Fatalf("release the competing lock: %v", err)
	}
	_, repository, release, err = openExisting(ctx, root)
	if err != nil {
		t.Fatalf("openExisting after the lock was released: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close refreshed index: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release the index lock: %v", err)
	}
	// The lock has to be free again, or every later command would block.
	unlock, acquired, err = service.TryIndexLock(project.IndexPath)
	if err != nil || !acquired {
		t.Fatalf("index lock was not released: acquired=%v err=%v", acquired, err)
	}
	_ = unlock()
}

// TestOpenReadReleasesTheIndexLockOnTheReadOnlyPath covers the close/reopen
// path: a query command drops the writable handle, reopens the index
// query-only, and must not leak the lock to the rest of the process.
func TestOpenReadReleasesTheIndexLockOnTheReadOnlyPath(t *testing.T) {
	t.Parallel()
	root := indexedRepository(t)
	ctx := context.Background()
	args, err := parseArguments([]string{"find", "Charge", "--repo", root})
	if err != nil {
		t.Fatal(err)
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRepository() }()
	if len(projects) != 1 {
		t.Fatalf("projects = %#v", projects)
	}
	unlock, acquired, err := service.TryIndexLock(projects[0].IndexPath)
	if err != nil || !acquired {
		t.Fatalf("openRead held the index lock past its refresh: acquired=%v err=%v", acquired, err)
	}
	_ = unlock()
	if nodes, err := repository.SearchNodes(ctx, "Charge", 10); err != nil || len(nodes) == 0 {
		t.Fatalf("query-only search = %#v, %v", nodes, err)
	}
}

// TestConcurrentQueryCommandsSerializeOnOneIndex is the regression for the
// reported failure: concurrent query commands against one branch index used to
// race as writers and die on SQLITE_BUSY or a half-applied migration. They now
// queue behind the lock, so every invocation answers.
func TestConcurrentQueryCommandsSerializeOnOneIndex(t *testing.T) {
	t.Parallel()
	root := indexedRepository(t)
	const invocations = 4
	codes := make([]int, invocations)
	var group sync.WaitGroup
	for i := range invocations {
		group.Add(1)
		go func() {
			defer group.Done()
			codes[i] = run(t, "find", "Charge", "--repo", root, "--json")
		}()
	}
	group.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("concurrent query %d exited with %d", i, code)
		}
	}
}
