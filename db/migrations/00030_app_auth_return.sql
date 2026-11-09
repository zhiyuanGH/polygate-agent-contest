-- +goose Up
ALTER TABLE auth_flows ADD COLUMN return_path text NOT NULL DEFAULT '/'
    CHECK (length(return_path) BETWEEN 1 AND 2300);

-- +goose Down
ALTER TABLE auth_flows DROP COLUMN return_path;
