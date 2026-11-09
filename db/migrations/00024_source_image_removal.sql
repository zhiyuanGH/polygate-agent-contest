-- +goose Up
-- Explicit student deletion may retire a built image without erasing the
-- source/build audit. The retained build becomes non-deployable by design.
ALTER TABLE source_build_tasks ADD COLUMN result_image_deleted_at timestamptz;
ALTER TABLE source_build_tasks DROP CONSTRAINT source_build_tasks_check;
ALTER TABLE source_build_tasks ADD CONSTRAINT source_build_tasks_check
    CHECK (state <> 'Succeeded' OR
        ((result_image_artifact_id IS NOT NULL) <> (result_image_deleted_at IS NOT NULL)));
ALTER TABLE source_build_tasks ADD CONSTRAINT source_build_tasks_removed_result_ck
    CHECK (result_image_deleted_at IS NULL OR state = 'Succeeded');

-- +goose Down
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM source_build_tasks WHERE result_image_deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back source image removal while retired build results exist';
    END IF;
END $$;
ALTER TABLE source_build_tasks DROP CONSTRAINT source_build_tasks_removed_result_ck;
ALTER TABLE source_build_tasks DROP CONSTRAINT source_build_tasks_check;
ALTER TABLE source_build_tasks ADD CONSTRAINT source_build_tasks_check
    CHECK (state <> 'Succeeded' OR result_image_artifact_id IS NOT NULL);
ALTER TABLE source_build_tasks DROP COLUMN result_image_deleted_at;
