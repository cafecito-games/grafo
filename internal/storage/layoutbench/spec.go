package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
)

// LayoutSpec describes one candidate SQLite physical layout as deltas from
// production: which Goose migrations build the schema, which named statements
// replace the production query text, and which write or read behaviors the
// shared adapter routes differently. The zero-delta spec (ProductionSpec) must
// behave exactly like the production adapter, which is what the equivalence
// driver proves before any layout measurement happens; candidates are small
// deltas on top of that proven core.
type LayoutSpec struct {
	// Name labels the layout in reports.
	Name string
	// Migrations builds the schema; Goose applies it at open time. Candidate
	// layouts mirror the production migration set with the same version
	// numbers so a pre-seeded database stays interchangeable.
	Migrations fs.FS
	// SQL overrides the text of one named statement (the -- name: values in
	// internal/storage/sqlite/queries/*.sql). Entries absent here fall back to
	// the production body mirrored by ProductionPlanQueries, whose drift test
	// keeps that mirror aligned with the sqlc sources. A name that is not a
	// production statement declares a candidate-only statement; every entry is
	// prepared when the adapter opens.
	//
	// Statements may use @name placeholders. The adapter numbers them by first
	// appearance and binds call-site values positionally, so an override that
	// needs one logical value twice (say a node id and the subselect that
	// resolves its integer key) names the parameter instead of duplicating the
	// value at the call site.
	SQL map[string]string
	// EdgeInsert replaces the default batched edge write during file
	// replacement and reconciliation. derived marks a DirectTestEdge
	// derivation, whose evidence is not the fact row.
	EdgeInsert EdgeInsertFunc
	// Hydrate replaces the default edge-row scan for adjacency queries
	// (ListEdgesFrom, ListEdgesTo, ListExternalEdgesMatching). The default
	// expects the production edge column order.
	Hydrate EdgeHydrationFunc
	// PostEdgeDelete runs inside the same transaction immediately before the
	// adapter deletes edges for one scope, so a layout with side tables can
	// mirror the deletion.
	PostEdgeDelete PostEdgeDeleteFunc
	// DerivedEvidence moves derived-edge evidence out of the edge row. When
	// true the adapter requires EdgeInsert and routes every edge write through
	// it with the derived flag.
	DerivedEvidence bool
	// IntegerKeys keys adjacency on integer node surrogates. When true the
	// adapter resolves a public node id to its surrogate once per adjacency
	// lookup through the ResolveNodeKey statement and binds the surrogate as
	// the subject of the (overridden) adjacency statements.
	IntegerKeys bool
}

// EdgeDeleteScope names the edge deletion a PostEdgeDelete hook precedes. The
// scope value is the production statement name whose predicate the hook must
// mirror.
type EdgeDeleteScope string

const (
	// EdgeDeleteDirtyFactBatch precedes DeleteEdgesByDirtyFactBatch; the
	// argument is the reconciliation batch bound.
	EdgeDeleteDirtyFactBatch EdgeDeleteScope = "DeleteEdgesByDirtyFactBatch"
	// EdgeDeleteOwnerFacts precedes DeleteEdgesByOwnerFacts; the argument is
	// the owner path.
	EdgeDeleteOwnerFacts EdgeDeleteScope = "DeleteEdgesByOwnerFacts"
)

// EdgeDeleteExecutor runs one named statement inside the active transaction.
type EdgeDeleteExecutor interface {
	ExecStatement(ctx context.Context, statementName string, values ...any) error
}

// PostEdgeDeleteFunc mirrors an edge deletion into layout-owned side tables.
// It runs immediately before the scoped production deletion, inside the same
// transaction, with the same bound arguments.
type PostEdgeDeleteFunc func(ctx context.Context, executor EdgeDeleteExecutor, scope EdgeDeleteScope, arguments []any) error

// StatementWriter buffers rows for one named single-row statement inside the
// active transaction, under the same bounded batch limits as the default
// write path.
type StatementWriter interface {
	AddStatement(ctx context.Context, statementName string, values ...any) error
}

// EdgeInsertFunc writes one edge row. derived is true only for DirectTestEdge
// derivations, whose evidence is not the fact row.
type EdgeInsertFunc func(ctx context.Context, writer StatementWriter, edge graph.Edge, derived bool) error

// EdgeHydrationFunc scans adjacency query rows into edges.
type EdgeHydrationFunc func(rows *sql.Rows) ([]graph.Edge, error)

// ResolveNodeKeyStatementName is the candidate-only statement an IntegerKeys
// layout must provide: it maps one public node id to its integer surrogate.
const ResolveNodeKeyStatementName = "ResolveNodeKey"

// ProductionSpec returns the zero-delta layout: production migrations and the
// production statement text everywhere.
func ProductionSpec(name string) LayoutSpec {
	return LayoutSpec{Name: name, Migrations: migrations.Files, SQL: map[string]string{}}
}

// statementText merges the spec overrides over the production bodies and
// fails closed on a spec that cannot run: missing migrations, an empty
// override, an integer-key layout without a key resolution statement, or a
// derived-evidence layout without an edge insert hook.
func (spec LayoutSpec) statementText() (map[string]string, error) {
	if spec.Migrations == nil {
		return nil, fmt.Errorf("layout %q has no migrations", spec.Name)
	}
	text := make(map[string]string, len(spec.SQL)+64)
	for _, query := range ProductionPlanQueries() {
		text[query.Name] = query.SQL
	}
	for name, statement := range spec.SQL {
		if strings.TrimSpace(statement) == "" {
			return nil, fmt.Errorf("layout %q overrides statement %s with empty SQL", spec.Name, name)
		}
		text[name] = statement
	}
	if spec.IntegerKeys {
		if _, found := text[ResolveNodeKeyStatementName]; !found {
			return nil, fmt.Errorf("layout %q uses integer keys but defines no %s statement",
				spec.Name, ResolveNodeKeyStatementName)
		}
	}
	if spec.DerivedEvidence && spec.EdgeInsert == nil {
		return nil, fmt.Errorf("layout %q separates derived evidence but provides no EdgeInsert hook", spec.Name)
	}
	return text, nil
}
