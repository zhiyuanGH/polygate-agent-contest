-- +goose Up
-- Persist exact backend identities before deleting a retained PVC. This lets
-- the controller resume safely after a crash or a partial Kubernetes delete.
ALTER TABLE student_volumes ADD COLUMN deletion_pvc_uid uuid;
ALTER TABLE student_volumes ADD COLUMN deletion_pv_name text;
ALTER TABLE student_volumes ADD COLUMN deletion_longhorn_handle text;
ALTER TABLE student_volumes ADD CONSTRAINT student_volume_deletion_targets_complete
    CHECK ((deletion_pvc_uid IS NULL AND deletion_pv_name IS NULL AND deletion_longhorn_handle IS NULL)
        OR (deletion_pvc_uid IS NOT NULL AND deletion_pv_name IS NOT NULL AND deletion_longhorn_handle IS NOT NULL));

-- +goose Down
ALTER TABLE student_volumes DROP CONSTRAINT student_volume_deletion_targets_complete;
ALTER TABLE student_volumes DROP COLUMN deletion_longhorn_handle;
ALTER TABLE student_volumes DROP COLUMN deletion_pv_name;
ALTER TABLE student_volumes DROP COLUMN deletion_pvc_uid;
