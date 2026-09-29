package layoutbench

import (
	"embed"
	"io/fs"
)

// slimEdgesEmbedded holds the slim-edges candidate's Goose migrations. The
// files mirror the production migration set with the same version numbers
// (the pre-seed trick: a database already migrated by production opens
// cleanly against the candidate), with the edges table replaced and
// derived_edge_evidence added.
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
