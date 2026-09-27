package service

import (
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
