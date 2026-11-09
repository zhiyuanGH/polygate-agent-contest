-- +goose Up
-- A Running app keeps its successful revision reservation while a replacement
-- rolls out. A separate reservation covers the temporary surge Pod. Failed
-- updates may enqueue a rollback task without replacing that successful
-- revision or releasing the surge budget before the old workload is observed.
ALTER TABLE deployment_tasks
    ADD COLUMN task_kind text NOT NULL DEFAULT 'Deploy'
        CHECK (task_kind IN ('Deploy', 'Update', 'Rollback', 'Stop')),
    ADD COLUMN prior_revision_id uuid REFERENCES app_revisions(id),
    ADD COLUMN rollback_of_task_id uuid REFERENCES deployment_tasks(id),
    ADD CONSTRAINT deployment_task_rollout_shape CHECK (
        (task_kind = 'Update' AND prior_revision_id IS NOT NULL AND rollback_of_task_id IS NULL)
        OR (task_kind = 'Rollback' AND prior_revision_id IS NULL AND rollback_of_task_id IS NOT NULL)
        OR (task_kind IN ('Deploy', 'Stop') AND prior_revision_id IS NULL AND rollback_of_task_id IS NULL)
    );

UPDATE deployment_tasks SET task_kind='Stop' WHERE desired_state='Stopped';

CREATE TABLE rollout_reservations (
    update_task_id uuid PRIMARY KEY REFERENCES deployment_tasks(id),
    account_id uuid NOT NULL REFERENCES accounts(id),
    app_id uuid NOT NULL REFERENCES apps(id),
    cpu_request_milli integer NOT NULL CHECK (cpu_request_milli > 0),
    cpu_limit_milli integer NOT NULL CHECK (cpu_limit_milli > 0),
    memory_mib integer NOT NULL CHECK (memory_mib > 0),
    pods integer NOT NULL DEFAULT 1 CHECK (pods = 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_id)
);
CREATE INDEX rollout_reservations_account_idx ON rollout_reservations(account_id);

-- +goose Down
DROP TABLE rollout_reservations;
ALTER TABLE deployment_tasks
    DROP CONSTRAINT deployment_task_rollout_shape,
    DROP COLUMN rollback_of_task_id,
    DROP COLUMN prior_revision_id,
    DROP COLUMN task_kind;
