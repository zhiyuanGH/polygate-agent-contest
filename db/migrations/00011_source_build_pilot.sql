-- +goose Up
-- Admit accounts to isolated ZIP building only after the host has passed its
-- live security and recovery checks. Existing accounts remain closed.
ALTER TABLE accounts
    ADD COLUMN source_build_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN source_build_enabled;
