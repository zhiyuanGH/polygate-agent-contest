-- +goose Up
-- Reserve quota and an artifact ID before writing the file. A Preparing task
-- is never claimable; it becomes Queued only after ZIP fsync and SHA-256.
CREATE TABLE source_artifacts (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    sha256 text CHECK (sha256 ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 104857600),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, account_id)
);
CREATE INDEX source_artifacts_account_idx ON source_artifacts(account_id, created_at DESC);

CREATE TABLE source_build_tasks (
    id uuid PRIMARY KEY,
    artifact_id uuid NOT NULL UNIQUE,
    account_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'Preparing'
        CHECK (state IN ('Preparing', 'Queued', 'Leased', 'Succeeded', 'Failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 3),
    locked_by text CHECK (locked_by IS NULL OR length(locked_by) BETWEEN 1 AND 128),
    lease_until timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    error_code text CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
    result_image_artifact_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (artifact_id, account_id)
        REFERENCES source_artifacts(id, account_id),
    FOREIGN KEY (result_image_artifact_id, account_id)
        REFERENCES image_artifacts(id, account_id),
    CHECK (state <> 'Succeeded' OR result_image_artifact_id IS NOT NULL),
    CHECK ((state = 'Leased') = (locked_by IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX source_build_tasks_ready_idx
    ON source_build_tasks(next_attempt_at, created_at, id)
    WHERE state IN ('Queued', 'Leased');
CREATE INDEX source_build_tasks_account_idx
    ON source_build_tasks(account_id, created_at DESC);

-- +goose Down
DROP TABLE source_build_tasks;
DROP TABLE source_artifacts;
