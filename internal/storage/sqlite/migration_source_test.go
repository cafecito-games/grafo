package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/pressly/goose/v3"
)

func TestRepositoryMigratesLegacyExactSourceFacts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO nodes(id, kind, name, qualified_name, owner_file) VALUES
    ('caller', 'function', 'caller', 'legacy.caller', 'legacy.go'),
    ('target', 'function', 'target', 'legacy.target', 'legacy.go');
INSERT INTO facts(id, from_id, kind, target_id, owner_file)
    VALUES ('legacy-fact', 'caller', 'calls', 'target', 'legacy.go');
INSERT INTO dirty_owners(owner_file) VALUES ('legacy.go');`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, "caller")
	if err != nil || len(edges) != 1 || edges[0].ToID != "target" || edges[0].Producer != "" {
		t.Fatalf("migrated legacy edge = %#v, err=%v", edges, err)
	}
}
