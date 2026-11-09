-- +goose Up
-- A cleanup task can finish as Stopped (user request) or Failed (deployment
-- exhausted its retry budget). Resource reservations remain until cleanup
-- confirms the Deployment, Service and Pods have disappeared.
ALTER TABLE deployment_tasks ADD COLUMN completion_status text NOT NULL DEFAULT 'Stopped'
    CHECK (completion_status IN ('Stopped', 'Failed'));

-- +goose Down
ALTER TABLE deployment_tasks DROP COLUMN completion_status;
