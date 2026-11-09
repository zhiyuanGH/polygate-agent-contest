-- +goose Up
-- A two-step delete keeps an image non-deployable if Harbor succeeds but the
-- database update is interrupted. A later GC run retries the same digest.
ALTER TABLE image_artifacts ADD COLUMN gc_state text NOT NULL DEFAULT 'Active'
    CHECK (gc_state IN ('Active', 'Deleting'));
ALTER TABLE image_artifacts ADD COLUMN gc_started_at timestamptz;
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_gc_state_ck
    CHECK ((gc_state = 'Deleting') = (gc_started_at IS NOT NULL));
CREATE INDEX image_artifacts_gc_candidate_idx
    ON image_artifacts(gc_state, scan_status, created_at, id);

-- +goose Down
DROP INDEX image_artifacts_gc_candidate_idx;
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_gc_state_ck;
ALTER TABLE image_artifacts DROP COLUMN gc_started_at;
ALTER TABLE image_artifacts DROP COLUMN gc_state;
