-- +goose Up
CREATE TABLE lexical_postings (term TEXT NOT NULL, node_id BLOB NOT NULL, PRIMARY KEY(term,node_id)) WITHOUT ROWID;
CREATE INDEX lexical_postings_node ON lexical_postings(node_id);
CREATE TABLE lexical_terms (term TEXT PRIMARY KEY, population INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE lexical_dirty (node_id BLOB PRIMARY KEY) WITHOUT ROWID;
CREATE INDEX nodes_lexical_file ON nodes(path,kind,id);
-- +goose StatementBegin
CREATE TRIGGER lexical_posting_insert AFTER INSERT ON lexical_postings BEGIN
 INSERT INTO lexical_terms VALUES(new.term,1) ON CONFLICT(term) DO UPDATE SET population=population+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lexical_posting_delete AFTER DELETE ON lexical_postings BEGIN
 UPDATE lexical_terms SET population=population-1 WHERE term=old.term;
 DELETE FROM lexical_terms WHERE term=old.term AND population=0;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lexical_node_insert AFTER INSERT ON nodes BEGIN
 INSERT INTO lexical_dirty VALUES(new.id) ON CONFLICT(node_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lexical_node_update AFTER UPDATE ON nodes BEGIN
 INSERT INTO lexical_dirty VALUES(new.id) ON CONFLICT(node_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lexical_node_delete AFTER DELETE ON nodes BEGIN
 DELETE FROM lexical_postings WHERE node_id=old.id;
 DELETE FROM lexical_dirty WHERE node_id=old.id;
END;
-- +goose StatementEnd
INSERT INTO lexical_dirty SELECT id FROM nodes WHERE external=0;
-- +goose Down
DROP TRIGGER lexical_node_delete;
DROP TRIGGER lexical_node_update;
DROP TRIGGER lexical_node_insert;
DROP TRIGGER lexical_posting_delete;
DROP TRIGGER lexical_posting_insert;
DROP INDEX nodes_lexical_file;
DROP TABLE lexical_dirty;
DROP TABLE lexical_terms;
DROP TABLE lexical_postings;
