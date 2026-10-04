package indexer_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// Go invalidation is scoped: a declaration edit reparses the edited package and
// its transitive importers rather than every Go file, and only the repository's
// type universe is still global. That scoping is correct exactly when an
// incremental pass produces the graph a cold index of the same tree would, so
// this test asserts that directly — every node and every fact, not just counts,
// because a count can match while a single edge points somewhere stale.
//
// Each case is an edit shape with a different blast radius: one that reaches
// only its own package, one that reaches importers, and one that reaches the
// whole repository through interface satisfaction.
func TestIncrementalEditsMatchAColdIndexRowForRow(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		files map[string]string
	}{
		{
			name: "body edit reaches only its own package",
			files: map[string]string{
				"base/base.go": "package base\n\nfunc Base() string { return \"changed body\" }\n",
			},
		},
		{
			name: "added function reaches importers",
			files: map[string]string{
				"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
			},
		},
		{
			name: "changed signature reaches importers",
			files: map[string]string{
				"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\ntype Record struct{ Name string }\n",
			},
		},
		{
			name: "added interface reaches every package",
			files: map[string]string{
				"contract/contract.go": "package contract\n\ntype Runner interface{ Run() error }\n\ntype Namer interface{ Name() string }\n",
			},
		},
		{
			name: "a type stops satisfying an interface",
			files: map[string]string{
				"worker/worker.go": "package worker\n\ntype Worker struct{}\n\nfunc (Worker) Execute() error { return nil }\n",
			},
		},
		{
			name: "a new type starts satisfying an interface",
			files: map[string]string{
				"unrelated/other.go": "package unrelated\n\ntype Extra struct{}\n\nfunc (Extra) Run() error { return nil }\n",
			},
		},
		{
			name: "a package stops parsing",
			files: map[string]string{
				"base/base.go": "package base\n\nfunc Base( string { return \"base\" }\n",
			},
		},
		{
			name:  "a dependency is deleted",
			files: map[string]string{"base/base.go": ""},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := equivalenceCorpus(t)
			project, err := indexer.DiscoverProject(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			// Both databases describe the same worktree. Node identifiers hash
			// the repository identity, which without an origin remote is the
			// root path, so comparing two checkouts would compare two different
			// repositories rather than two ways of indexing one.
			incrementalPath := filepath.Join(root, ".grafo", "indexes", "incremental.sqlite")
			coldPath := filepath.Join(root, ".grafo", "indexes", "cold.sqlite")

			// The incremental database is built against the original tree, so
			// the edit below is reconciled rather than indexed from nothing.
			runEquivalenceIndex(t, ctx, project, incrementalPath)
			applyEquivalenceEdit(t, root, testCase.files)

			incrementalReport := runEquivalenceIndex(t, ctx, project, incrementalPath)
			runEquivalenceIndex(t, ctx, project, coldPath)

			for _, table := range []string{"nodes", "facts"} {
				incremental := tableFingerprint(t, ctx, incrementalPath, table)
				cold := tableFingerprint(t, ctx, coldPath, table)
				if incremental != cold {
					onlyIncremental, onlyCold := rowDifference(incremental, cold)
					t.Fatalf("%s diverged from a cold index after %q (reparsed %d files)\nonly incremental (%d):\n%s\nonly cold (%d):\n%s",
						table, testCase.name, len(incrementalReport.Updated),
						len(onlyIncremental), strings.Join(onlyIncremental, "\n"),
						len(onlyCold), strings.Join(onlyCold, "\n"))
				}
			}
		})
	}
}

// TestDeclarationEditDoesNotReparseUnreachablePackages is the performance half
// of the same contract. Equivalence alone is satisfied by reparsing everything,
// which is what this change exists to stop.
func TestDeclarationEditDoesNotReparseUnreachablePackages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := equivalenceCorpus(t)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runEquivalenceIndex(t, ctx, project, project.IndexPath)

	applyEquivalenceEdit(t, root, map[string]string{
		"base/base.go": "package base\n\nfunc Base() string { return \"base\" }\n\nfunc Added() int { return 1 }\n",
	})
	report := runEquivalenceIndex(t, ctx, project, project.IndexPath)

	reparsed := map[string]bool{}
	for _, path := range report.Updated {
		reparsed[path] = true
	}
	// base is edited and middle imports it, so both are reachable. unrelated
	// imports nothing from base, so reparsing it would be wasted work.
	for _, path := range []string{"base/base.go", "middle/middle.go"} {
		if !reparsed[path] {
			t.Fatalf("%s was not reparsed; updated = %v", path, report.Updated)
		}
	}
	if reparsed["unrelated/other.go"] {
		t.Fatalf("an unreachable package was reparsed; updated = %v", report.Updated)
	}
}

// equivalenceCorpus writes a Git corpus with a dependency chain and an
// interface satisfied from another package, so an edit can be aimed at each
// blast radius in turn.
func equivalenceCorpus(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/equivalence\n\ngo 1.26\n")
	write(t, filepath.Join(root, "base", "base.go"),
		"package base\n\nfunc Base() string { return \"base\" }\n")
	write(t, filepath.Join(root, "middle", "middle.go"),
		"package middle\n\nimport \"example.com/equivalence/base\"\n\nfunc Middle() string { return base.Base() }\n")
	write(t, filepath.Join(root, "contract", "contract.go"),
		"package contract\n\ntype Runner interface{ Run() error }\n")
	// worker satisfies contract.Runner without importing it, which is the case
	// that keeps the type universe repository wide.
	write(t, filepath.Join(root, "worker", "worker.go"),
		"package worker\n\ntype Worker struct{}\n\nfunc (Worker) Run() error { return nil }\n")
	write(t, filepath.Join(root, "unrelated", "other.go"),
		"package unrelated\n\nfunc Other() string { return \"other\" }\n")
	return root
}

func applyEquivalenceEdit(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(t, path, content)
	}
}

func runEquivalenceIndex(t *testing.T, ctx context.Context, project indexer.Project, indexPath string) indexer.Report {
	t.Helper()
	repository, err := sqlite.Open(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// tableFingerprint renders every row of a graph table in identifier order. The
// whole rendering is returned rather than a digest so a failure names the
// divergence instead of only reporting that one exists.
//
// Interned path identifiers are resolved to their paths. They are surrogate
// keys assigned in insertion order, so an incrementally built database numbers
// them differently from a freshly built one while describing the same graph;
// comparing the numbers would report a difference that does not exist.
func tableFingerprint(t *testing.T, ctx context.Context, indexPath, table string) string {
	t.Helper()
	database, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	interned := internedPaths(t, ctx, database)
	// edges is keyed by the columns its identity is derived from rather than by a
	// stored id, so each table is ordered by whatever is total for it.
	order := map[string]string{"edges": "fact_id, to_id, kind"}[table]
	if order == "" {
		order = "id"
	}
	rows, err := database.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	rendered := ""
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for index, column := range columns {
			value := values[index]
			if column == "path_id" || column == "owner_path_id" {
				column = strings.TrimSuffix(column, "_id")
				value = interned[fmt.Sprint(value)]
			}
			rendered += fmt.Sprintf("%s=%v\x1f", column, value)
		}
		rendered += "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return rendered
}

func rowDifference(left, right string) ([]string, []string) {
	leftRows := map[string]bool{}
	for _, row := range strings.Split(left, "\n") {
		if row != "" {
			leftRows[row] = true
		}
	}
	rightRows := map[string]bool{}
	for _, row := range strings.Split(right, "\n") {
		if row != "" {
			rightRows[row] = true
		}
	}
	var onlyLeft, onlyRight []string
	for row := range leftRows {
		if !rightRows[row] {
			onlyLeft = append(onlyLeft, row)
		}
	}
	for row := range rightRows {
		if !leftRows[row] {
			onlyRight = append(onlyRight, row)
		}
	}
	sort.Strings(onlyLeft)
	sort.Strings(onlyRight)
	return onlyLeft, onlyRight
}

// internedPaths reads the surrogate path table so a row's path identifier can be
// rendered as the path it names.
func internedPaths(t *testing.T, ctx context.Context, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.QueryContext(ctx, "SELECT id, path FROM paths")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]string{}
	for rows.Next() {
		var identifier int64
		var path string
		if err := rows.Scan(&identifier, &path); err != nil {
			t.Fatal(err)
		}
		result[fmt.Sprint(identifier)] = path
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
