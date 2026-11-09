-- +goose Up
ALTER TABLE apps ADD COLUMN secret_keys text[] NOT NULL DEFAULT '{}'::text[]
    CHECK (cardinality(secret_keys) <= 8);

-- +goose Down
ALTER TABLE apps DROP COLUMN secret_keys;
