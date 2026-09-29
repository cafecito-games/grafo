-- +goose Up
ALTER TABLE facts ADD COLUMN producer TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE facts DROP COLUMN producer;
