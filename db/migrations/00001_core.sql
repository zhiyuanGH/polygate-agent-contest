-- +goose Up
CREATE TABLE accounts (
    id uuid PRIMARY KEY,
    oidc_subject text NOT NULL UNIQUE,
    username text NOT NULL UNIQUE,
    display_name text NOT NULL,
    role text NOT NULL DEFAULT 'student' CHECK (role IN ('student', 'admin')),
    enabled boolean NOT NULL DEFAULT true,
    quota_cpu_request_milli integer NOT NULL DEFAULT 2000 CHECK (quota_cpu_request_milli >= 0),
    quota_cpu_limit_milli integer NOT NULL DEFAULT 4000 CHECK (quota_cpu_limit_milli >= 0),
    quota_memory_mib integer NOT NULL DEFAULT 4096 CHECK (quota_memory_mib >= 0),
    quota_pods integer NOT NULL DEFAULT 4 CHECK (quota_pods >= 0),
    quota_apps integer NOT NULL DEFAULT 2 CHECK (quota_apps >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE apps (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    name text NOT NULL,
    image text NOT NULL,
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    cpu_request_milli integer NOT NULL CHECK (cpu_request_milli > 0),
    cpu_limit_milli integer NOT NULL CHECK (cpu_limit_milli >= cpu_request_milli),
    memory_mib integer NOT NULL CHECK (memory_mib > 0),
    replicas integer NOT NULL CHECK (replicas BETWEEN 1 AND 2),
    persistent boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'Draft' CHECK (status IN ('Draft', 'Queued', 'Building', 'Deploying', 'Running', 'Stopped', 'Failed', 'Deleting')),
    idempotency_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, name),
    UNIQUE (account_id, idempotency_key),
    CHECK (NOT persistent OR replicas = 1)
);
CREATE INDEX apps_account_created_idx ON apps (account_id, created_at, id);

CREATE TABLE operations (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    app_id uuid REFERENCES apps(id) ON DELETE SET NULL,
    actor_subject text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX operations_account_created_idx ON operations (account_id, created_at DESC);

CREATE TABLE auth_sessions (
    session_hash bytea PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    csrf_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_sessions_expiry_idx ON auth_sessions (expires_at);

CREATE TABLE auth_flows (
    flow_hash bytea PRIMARY KEY,
    state_hash bytea NOT NULL,
    nonce_hash bytea NOT NULL,
    pkce_verifier text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_flows_expiry_idx ON auth_flows (expires_at);

-- The global row is reserved for deployments. Drafts do not consume it.
CREATE TABLE global_budget (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    cpu_request_milli integer NOT NULL DEFAULT 60000,
    cpu_limit_milli integer NOT NULL DEFAULT 80000,
    memory_mib integer NOT NULL DEFAULT 102400,
    pods integer NOT NULL DEFAULT 60,
    reserved_cpu_request_milli integer NOT NULL DEFAULT 0,
    reserved_cpu_limit_milli integer NOT NULL DEFAULT 0,
    reserved_memory_mib integer NOT NULL DEFAULT 0,
    reserved_pods integer NOT NULL DEFAULT 0
);
INSERT INTO global_budget (id) VALUES (true);

-- +goose Down
DROP TABLE global_budget;
DROP TABLE auth_flows;
DROP TABLE auth_sessions;
DROP TABLE operations;
DROP TABLE apps;
DROP TABLE accounts;
