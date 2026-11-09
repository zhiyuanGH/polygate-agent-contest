-- +goose Up
-- A restart is an update of the same revision content with a new Pod template.
ALTER TABLE deployment_tasks DROP CONSTRAINT deployment_tasks_update_kind_check;
ALTER TABLE deployment_tasks ADD CONSTRAINT deployment_tasks_update_kind_check
    CHECK (update_kind IN ('Image', 'Resources', 'Restart'));

-- +goose Down
ALTER TABLE deployment_tasks DROP CONSTRAINT deployment_tasks_update_kind_check;
ALTER TABLE deployment_tasks ADD CONSTRAINT deployment_tasks_update_kind_check
    CHECK (update_kind IN ('Image', 'Resources'));
