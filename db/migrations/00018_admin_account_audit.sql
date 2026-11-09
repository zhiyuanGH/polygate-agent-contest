-- +goose Up
CREATE TABLE admin_account_audit (
    id uuid PRIMARY KEY,
    actor_account_id uuid NOT NULL REFERENCES accounts(id),
    target_account_id uuid NOT NULL REFERENCES accounts(id),
    actor_subject text NOT NULL,
    action text NOT NULL CHECK (action IN ('ENABLE_ACCOUNT', 'DISABLE_ACCOUNT')),
    reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 3 AND 500),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_account_audit_created_idx ON admin_account_audit (created_at DESC, id DESC);
CREATE INDEX admin_account_audit_target_idx ON admin_account_audit (target_account_id, created_at DESC);

-- +goose Down
DROP TABLE admin_account_audit;
