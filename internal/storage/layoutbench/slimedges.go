package layoutbench

import (
	"embed"
	"io/fs"
)

// slimEdgesEmbedded holds the slim-edges candidate's Goose migrations. The
// files mirror the production migration set with the same version numbers, so
// Goose treats migration as a no-op on a version-matched database; a database
// whose schema is otherwise incompatible (say one seeded by production, which
// lacks derived_edge_evidence) fails closed at statement preparation.
//
//go:embed variants/slimedges/*.sql
var slimEdgesEmbedded embed.FS

// slimEdgesMigrations is the migration filesystem OpenVariant hands to Goose.
var slimEdgesMigrations = func() fs.FS {
	migrations, err := fs.Sub(slimEdgesEmbedded, "variants/slimedges")
	if err != nil {
		// The embedded directory is fixed at compile time, so a failed sub
		// walk means the embed directive and this path disagree.
		panic("layoutbench: embed slim-edges migrations: " + err.Error())
	}
	return migrations
}()
