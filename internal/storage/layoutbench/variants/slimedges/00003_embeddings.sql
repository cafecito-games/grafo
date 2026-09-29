-- +goose Up
CREATE TABLE embeddings (
    node_id TEXT NOT NULL,
    model TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    dimensions INTEGER NOT NULL,
    vector_json TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, model)
);

CREATE INDEX embeddings_model ON embeddings(model, node_id);

-- +goose Down
DROP INDEX IF EXISTS embeddings_model;
DROP TABLE IF EXISTS embeddings;
