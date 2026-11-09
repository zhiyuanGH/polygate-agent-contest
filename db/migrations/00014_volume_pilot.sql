-- +goose Up
ALTER TABLE accounts ADD COLUMN volume_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN volume_enabled;
