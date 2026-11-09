-- +goose Up
-- Resource changes share the revision and automatic rollback task path with
-- image updates, while carrying an explicit rollout strategy and operation kind.
ALTER TABLE deployment_tasks
    ADD COLUMN update_kind text NOT NULL DEFAULT 'Image'
        CHECK (update_kind IN ('Image', 'Resources')),
    ADD COLUMN rollout_strategy text NOT NULL DEFAULT 'Rolling'
        CHECK (rollout_strategy IN ('Rolling', 'Recreate'));

ALTER TABLE rollout_reservations
    DROP CONSTRAINT rollout_reservations_pods_check,
    ADD CONSTRAINT rollout_reservations_pods_check CHECK (pods BETWEEN 1 AND 2);

-- +goose Down
ALTER TABLE rollout_reservations
    DROP CONSTRAINT rollout_reservations_pods_check,
    ADD CONSTRAINT rollout_reservations_pods_check CHECK (pods = 1);
ALTER TABLE deployment_tasks
    DROP COLUMN rollout_strategy,
    DROP COLUMN update_kind;
