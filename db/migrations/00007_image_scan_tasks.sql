-- +goose Up
-- One durable scan task per inspected digest. A worker lease may expire, but
-- only the current lease holder may acknowledge or retry the task.
CREATE TABLE image_scan_tasks (
    artifact_id uuid PRIMARY KEY,
    account_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'Queued'
        CHECK (state IN ('Queued', 'Leased', 'Succeeded', 'Failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 8),
    locked_by text CHECK (locked_by IS NULL OR length(locked_by) BETWEEN 1 AND 128),
    lease_until timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    error_code text CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (artifact_id, account_id)
        REFERENCES image_artifacts(id, account_id) ON DELETE CASCADE,
    CHECK ((state = 'Leased') = (locked_by IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX image_scan_tasks_ready_idx
    ON image_scan_tasks(next_attempt_at, created_at, artifact_id)
    WHERE state IN ('Queued', 'Leased');

-- Existing pending inspections are safe to rescan because they have no
-- approval evidence and cannot currently enter the deployment queue.
INSERT INTO image_scan_tasks(artifact_id, account_id)
SELECT id, account_id FROM image_artifacts WHERE scan_status = 'PendingScan';

-- +goose Down
DROP TABLE image_scan_tasks;
