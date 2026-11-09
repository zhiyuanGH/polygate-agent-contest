-- +goose Up
CREATE TABLE account_quota_changes (
    id uuid PRIMARY KEY,
    actor_account_id uuid NOT NULL REFERENCES accounts(id),
    target_account_id uuid NOT NULL REFERENCES accounts(id),
    actor_subject text NOT NULL,
    idempotency_key text NOT NULL,
    reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 3 AND 500),
    quota_cpu_request_milli integer NOT NULL CHECK (quota_cpu_request_milli >= 250),
    quota_cpu_limit_milli integer NOT NULL CHECK (quota_cpu_limit_milli >= 1000),
    quota_memory_mib integer NOT NULL CHECK (quota_memory_mib >= 1024),
    quota_pods integer NOT NULL CHECK (quota_pods >= 1),
    quota_apps integer NOT NULL CHECK (quota_apps >= 1),
    state text NOT NULL DEFAULT 'Queued' CHECK (state IN ('Queued','Applied')),
    attempt integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    applied_at timestamptz,
    CONSTRAINT quota_cpu_order CHECK (quota_cpu_request_milli <= quota_cpu_limit_milli),
    CONSTRAINT quota_key_unique UNIQUE (actor_account_id,idempotency_key)
);
CREATE UNIQUE INDEX account_quota_one_pending_idx ON account_quota_changes (target_account_id) WHERE state='Queued';
CREATE INDEX account_quota_created_idx ON account_quota_changes (created_at DESC,id DESC);

-- +goose Down
DROP TABLE account_quota_changes;
