package sqlite

import (
	"context"
	"fmt"
	"sync"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

// pathInterner caches the integer key the paths table assigns to each distinct
// owner key a fact can carry. Facts reference their location and owner by key,
// so one repository-relative path is stored once for the whole index instead of
// once per fact and once per edge.
type pathInterner struct {
	mu   sync.Mutex
	keys map[string]int64
}

func (i *pathInterner) lookup(path string) (int64, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	key, ok := i.keys[path]
	return key, ok
}

// publish adopts the keys a committed transaction assigned. Keys only become
// shared after the transaction that wrote their rows commits.
func (i *pathInterner) publish(assigned map[string]int64) {
	if len(assigned) == 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.keys == nil {
		i.keys = make(map[string]int64, len(assigned))
	}
	for path, key := range assigned {
		i.keys[path] = key
	}
}

// discard drops every cached key. Pruning unreferenced path rows can delete a
// row this cache still remembers, and reusing that key would attach facts to a
// row that no longer exists.
func (i *pathInterner) discard() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys = nil
}

func (i *pathInterner) begin() *pathTransaction {
	return &pathTransaction{interner: i}
}

// pathTransaction resolves path keys within one database transaction. Keys it
// assigns stay local until the transaction commits, so a rollback cannot leave
// the shared cache pointing at a row that was never written.
type pathTransaction struct {
	interner *pathInterner
	assigned map[string]int64
}

func (t *pathTransaction) key(ctx context.Context, q *sqlcgen.Queries, path string) (int64, error) {
	if key, ok := t.assigned[path]; ok {
		return key, nil
	}
	if cached, ok := t.interner.lookup(path); ok {
		// Another writable connection on the same index file can have pruned
		// the row this cache remembers. Confirming the key inside this
		// transaction keeps a stale cache from attaching facts to a row that no
		// longer exists, which no later re-index could repair.
		confirmed, err := q.ConfirmPathKey(ctx, sqlcgen.ConfirmPathKeyParams{ID: cached, Path: path})
		if err != nil {
			return 0, fmt.Errorf("confirm interned path %q: %w", path, err)
		}
		if confirmed {
			t.remember(path, cached)
			return cached, nil
		}
	}
	key, err := q.InternPath(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("intern path %q: %w", path, err)
	}
	t.remember(path, key)
	return key, nil
}

func (t *pathTransaction) remember(path string, key int64) {
	if t.assigned == nil {
		t.assigned = map[string]int64{}
	}
	t.assigned[path] = key
}
