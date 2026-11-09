-- +goose Up
ALTER TABLE apps ADD COLUMN volume_id uuid;
ALTER TABLE apps ADD COLUMN mount_path text;
ALTER TABLE apps ADD CONSTRAINT apps_account_volume_fk
    FOREIGN KEY (account_id, volume_id) REFERENCES student_volumes(account_id, id);
CREATE UNIQUE INDEX apps_volume_exclusive_idx ON apps(volume_id) WHERE volume_id IS NOT NULL;
ALTER TABLE apps ADD CONSTRAINT apps_volume_binding_consistent
    CHECK ((persistent AND volume_id IS NOT NULL AND mount_path IS NOT NULL)
        OR (NOT persistent AND volume_id IS NULL AND mount_path IS NULL));

-- +goose Down
ALTER TABLE apps DROP CONSTRAINT apps_volume_binding_consistent;
DROP INDEX apps_volume_exclusive_idx;
ALTER TABLE apps DROP CONSTRAINT apps_account_volume_fk;
ALTER TABLE apps DROP COLUMN mount_path;
ALTER TABLE apps DROP COLUMN volume_id;
