package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestAddCanonicalizesAndIsIdempotent(t *testing.T) {
	env := isolatedEnvironment(t)
	store := NewStore(env)
	root := testtemp.Dir(t)
	first, added, err := store.Add(root, Settings{})
	if err != nil || !added {
		t.Fatalf("Add(%q) = %v, added %v", root, err, added)
	}
	if len(first.Roots) != 1 {
		t.Fatalf("expected one root, got %#v", first.Roots)
	}
	if !filepath.IsAbs(first.Roots[0].Root) {
		t.Fatalf("root %q is not absolute", first.Roots[0].Root)
	}
	second, added, err := store.Add(root+string(filepath.Separator)+".", Settings{})
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if added {
		t.Fatalf("re-adding the same root must be idempotent")
	}
	if len(second.Roots) != 1 {
		t.Fatalf("expected one root after re-add, got %#v", second.Roots)
	}
	path, err := store.Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("registry mode = %v, want 0600", mode)
	}
}

func TestLoadRejectsMalformedRegistryWithRecoveryInstructions(t *testing.T) {
	env := isolatedEnvironment(t)
	store := NewStore(env)
	path, err := store.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load()
	if err == nil {
		t.Fatal("expected malformed registry to fail closed")
	}
	if !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("error %q does not report recovery instructions", err)
	}
}
