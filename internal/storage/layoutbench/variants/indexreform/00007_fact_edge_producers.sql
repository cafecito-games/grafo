-- +goose Up
ALTER TABLE facts ADD COLUMN producer TEXT NOT NULL DEFAULT '';
ALTER TABLE edges ADD COLUMN producer TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE edges DROP COLUMN producer;
ALTER TABLE facts DROP COLUMN producer;
