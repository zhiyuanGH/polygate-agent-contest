-- +goose Up
-- A reservation is not a PVC. Provisioning changes state only after a
-- controller has observed the account-owned claim in Kubernetes.
CREATE TABLE student_volumes (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,38}[a-z0-9]$|^[a-z]$'),
    size_gib integer NOT NULL CHECK (size_gib BETWEEN 1 AND 10),
    state text NOT NULL DEFAULT 'Reserved' CHECK (state IN ('Reserved', 'Provisioning', 'Ready', 'Expanding', 'Failed', 'Deleting')),
    idempotency_key text NOT NULL,
    lease_until timestamptz,
    locked_by text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, name),
    UNIQUE (account_id, idempotency_key),
    UNIQUE (account_id, id)
);
CREATE INDEX student_volumes_account_created_idx ON student_volumes(account_id, created_at, id);

-- +goose Down
DROP TABLE student_volumes;
