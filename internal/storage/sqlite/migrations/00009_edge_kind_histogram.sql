-- +goose Up
-- The full-graph edge histogram groups every row by kind. Without an index
-- leading with kind, SQLite scans a covering index and spills the GROUP BY into
-- its external merge sorter, which makes reporting cost grow with total graph
-- size. Leading with kind lets the aggregate stream in index order.
CREATE INDEX edges_kind ON edges(kind);

-- +goose Down
DROP INDEX IF EXISTS edges_kind;
