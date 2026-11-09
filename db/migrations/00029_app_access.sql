-- +goose Up
ALTER TABLE apps ADD COLUMN access_mode text NOT NULL DEFAULT 'Public'
    CHECK (access_mode IN ('Public', 'Owner and administrators'));

CREATE TABLE app_access_tickets (
    ticket_hash bytea PRIMARY KEY,
    app_id uuid NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    return_path text NOT NULL CHECK (length(return_path) BETWEEN 1 AND 2048),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX app_access_tickets_expiry_idx ON app_access_tickets (expires_at);

CREATE TABLE app_access_sessions (
    session_hash bytea PRIMARY KEY,
    app_id uuid NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX app_access_sessions_app_account_idx ON app_access_sessions (app_id, account_id);
CREATE INDEX app_access_sessions_expiry_idx ON app_access_sessions (expires_at);

-- +goose Down
DROP TABLE app_access_sessions;
DROP TABLE app_access_tickets;
ALTER TABLE apps DROP COLUMN access_mode;
