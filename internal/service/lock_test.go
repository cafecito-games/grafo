package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "guard.lock")
	unlock, err := Lock(path, time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := Lock(path, 50*time.Millisecond); err == nil {
		t.Fatal("expected the second Lock to fail while the first is held")
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	again, err := Lock(path, time.Second)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}

func TestTryIndexLockDoesNotWaitAndLeavesAnchor(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "indexes", "branch.sqlite")
	first, acquired, err := TryIndexLock(indexPath)
	if err != nil || !acquired {
		t.Fatalf("first TryIndexLock = acquired %t, err %v", acquired, err)
	}
	if second, acquired, err := TryIndexLock(indexPath); err != nil || acquired || second != nil {
		t.Fatalf("contended TryIndexLock = unlock %#v, acquired %t, err %v", second, acquired, err)
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath + ".lock"); err != nil {
		t.Fatalf("lock anchor was not retained: %v", err)
	}
}

func TestPortableLockUsesTransientSentinelAndPermanentAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portable.lock")
	first, acquired, err := tryPortableLock(path)
	if err != nil || !acquired {
		t.Fatalf("first portable lock = acquired %t, err %v", acquired, err)
	}
	if second, acquired, err := tryPortableLock(path); err != nil || acquired || second != nil {
		t.Fatalf("contended portable lock = unlock %#v, acquired %t, err %v", second, acquired, err)
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("portable lock removed permanent anchor: %v", err)
	}
	if _, err := os.Stat(path + ".held"); !os.IsNotExist(err) {
		t.Fatalf("portable lock sentinel survived unlock: %v", err)
	}
	again, acquired, err := tryPortableLock(path)
	if err != nil || !acquired {
		t.Fatalf("portable lock after release = acquired %t, err %v", acquired, err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}

func TestMutationFailsClosedWhenRegistryIsLocked(t *testing.T) {
	env := isolatedEnvironment(t)
	store := NewStore(env)
	path, err := store.Path()
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(path+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	store.wait = 20 * time.Millisecond
	if _, _, err := store.Add(t.TempDir(), Settings{}); err == nil {
		t.Fatal("expected Add to fail closed while the registry is locked")
	}
}
