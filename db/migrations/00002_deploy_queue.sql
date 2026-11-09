-- +goose Up
-- The first deployment path accepts only a platform selected image digest.
CREATE TABLE app_revisions (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    app_id uuid NOT NULL REFERENCES apps(id),
    revision integer NOT NULL CHECK (revision > 0),
    platform_image_digest text NOT NULL CHECK (platform_image_digest LIKE '%@sha256:%'),
    spec jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_id, revision),
    UNIQUE (id, app_id, account_id)
);

CREATE TABLE resource_reservations (
    app_id uuid PRIMARY KEY REFERENCES apps(id),
    account_id uuid NOT NULL REFERENCES accounts(id),
    revision_id uuid NOT NULL REFERENCES app_revisions(id),
    cpu_request_milli integer NOT NULL CHECK (cpu_request_milli > 0),
    cpu_limit_milli integer NOT NULL CHECK (cpu_limit_milli > 0),
    memory_mib integer NOT NULL CHECK (memory_mib > 0),
    pods integer NOT NULL CHECK (pods > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (revision_id)
);
CREATE INDEX resource_reservations_account_idx ON resource_reservations(account_id);

CREATE TABLE deployment_tasks (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    app_id uuid NOT NULL REFERENCES apps(id),
    revision_id uuid NOT NULL REFERENCES app_revisions(id),
    idempotency_key text NOT NULL,
    desired_state text NOT NULL CHECK (desired_state IN ('Running', 'Stopped')),
    state text NOT NULL DEFAULT 'Queued' CHECK (state IN ('Queued', 'Leased', 'Succeeded', 'Failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_until timestamptz,
    locked_by text,
    error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key)
);
CREATE UNIQUE INDEX deployment_tasks_active_app_idx ON deployment_tasks(app_id)
    WHERE state IN ('Queued', 'Leased');
CREATE INDEX deployment_tasks_claim_idx ON deployment_tasks(state, lease_until, created_at);

CREATE TABLE outbox (
    id uuid PRIMARY KEY,
    task_id uuid NOT NULL UNIQUE REFERENCES deployment_tasks(id),
    event_type text NOT NULL CHECK (event_type IN ('RECONCILE_DEPLOYMENT')),
    state text NOT NULL DEFAULT 'Pending' CHECK (state IN ('Pending', 'Dispatched')),
    created_at timestamptz NOT NULL DEFAULT now(),
    dispatched_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox(created_at) WHERE state = 'Pending';

-- +goose Down
DROP TABLE outbox;
DROP TABLE deployment_tasks;
DROP TABLE resource_reservations;
DROP TABLE app_revisions;
