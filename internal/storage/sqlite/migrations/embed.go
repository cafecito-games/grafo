package migrations

import "embed"

// Files contains every ordered Goose migration shipped with Grafo.
//
//go:embed *.sql
var Files embed.FS
