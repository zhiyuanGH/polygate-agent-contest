-- +goose Up
-- A student may request deletion, but the restricted GC worker performs the
-- Harbor operation after the account row has been locked and references checked.
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_gc_state_check;
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_gc_state_check
    CHECK (gc_state IN ('Active', 'Requested', 'Deleting'));
ALTER TABLE image_artifacts ADD COLUMN gc_reason text NOT NULL DEFAULT 'Retention'
    CHECK (gc_reason IN ('Retention', 'Student'));

-- +goose Down
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_gc_state_check;
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_gc_state_check
    CHECK (gc_state IN ('Active', 'Deleting'));
ALTER TABLE image_artifacts DROP COLUMN gc_reason;
