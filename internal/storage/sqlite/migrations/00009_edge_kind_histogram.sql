-- +goose Up
-- The full-graph edge histogram groups every row by kind. Without an index
-- leading with kind, SQLite scans a covering index and spills the GROUP BY into
-- its external merge sorter, which makes reporting cost grow with total graph
-- size. Leading with kind lets the aggregate stream in index order.
-- IF NOT EXISTS is defence in depth: two processes could both read schema
-- version 8 and both run this migration, and the loser should not die on an
-- index the winner already created.
CREATE INDEX IF NOT EXISTS edges_kind ON edges(kind);

-- +goose Down
DROP INDEX IF EXISTS edges_kind;
