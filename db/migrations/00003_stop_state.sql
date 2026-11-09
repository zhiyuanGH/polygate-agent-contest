-- +goose Up
ALTER TABLE apps DROP CONSTRAINT apps_status_check;
ALTER TABLE apps ADD CONSTRAINT apps_status_check
    CHECK (status IN ('Draft', 'Queued', 'Building', 'Deploying', 'Running',
                     'Stopping', 'Stopped', 'Failed', 'Deleting'));

-- +goose Down
UPDATE apps SET status='Failed' WHERE status='Stopping';
ALTER TABLE apps DROP CONSTRAINT apps_status_check;
ALTER TABLE apps ADD CONSTRAINT apps_status_check
    CHECK (status IN ('Draft', 'Queued', 'Building', 'Deploying', 'Running',
                     'Stopped', 'Failed', 'Deleting'));
