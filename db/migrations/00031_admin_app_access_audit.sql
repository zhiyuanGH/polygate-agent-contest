-- +goose Up
CREATE TABLE admin_app_access_audit (
    id uuid PRIMARY KEY,
    actor_account_id uuid NOT NULL,
    actor_subject text NOT NULL,
    owner_account_id uuid NOT NULL,
    app_id uuid NOT NULL,
    app_name text NOT NULL,
    previous_mode text NOT NULL,
    new_mode text NOT NULL,
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (previous_mode IN ('Public', 'Owner and administrators')),
    CHECK (new_mode IN ('Public', 'Owner and administrators')),
    CHECK (length(reason) BETWEEN 3 AND 500)
);
CREATE INDEX admin_app_access_audit_created_idx ON admin_app_access_audit (created_at DESC, id DESC);

-- +goose Down
DROP TABLE admin_app_access_audit;
