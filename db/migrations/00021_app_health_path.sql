-- +goose Up
ALTER TABLE apps ADD COLUMN health_path text NOT NULL DEFAULT ''
    CHECK (length(health_path) <= 128);

-- +goose Down
ALTER TABLE apps DROP COLUMN health_path;
