-- +goose Up
-- A resolved image is owned by one account. An inspection never grants deploy
-- permission: only a separate scanner may move PendingScan to Approved.
CREATE TABLE image_artifacts (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts(id),
    submitted_reference text NOT NULL CHECK (length(submitted_reference) BETWEEN 1 AND 512),
    pinned_reference text NOT NULL CHECK (length(pinned_reference) BETWEEN 1 AND 600 AND pinned_reference LIKE '%@sha256:%'),
    platform_digest text NOT NULL CHECK (platform_digest ~ '^sha256:[0-9a-f]{64}$'),
    registry_host text NOT NULL CHECK (length(registry_host) BETWEEN 1 AND 253),
    configured_user text NOT NULL DEFAULT '' CHECK (length(configured_user) <= 256),
    scan_status text NOT NULL DEFAULT 'PendingScan'
        CHECK (scan_status IN ('PendingScan', 'Approved', 'Rejected')),
    scan_report_sha256 text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (scan_status = 'PendingScan' OR scan_report_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    UNIQUE (account_id, pinned_reference),
    UNIQUE (id, account_id)
);
CREATE INDEX image_artifacts_account_created_idx ON image_artifacts(account_id, created_at DESC);

ALTER TABLE app_revisions ADD COLUMN source_kind text NOT NULL DEFAULT 'trusted'
    CHECK (source_kind IN ('trusted', 'image', 'source'));
ALTER TABLE app_revisions ADD COLUMN image_artifact_id uuid;
ALTER TABLE app_revisions ADD CONSTRAINT app_revisions_image_owner_fk
    FOREIGN KEY (image_artifact_id, account_id) REFERENCES image_artifacts(id, account_id);
ALTER TABLE app_revisions ADD CONSTRAINT app_revisions_source_artifact_ck
    CHECK ((source_kind = 'trusted' AND image_artifact_id IS NULL) OR
           (source_kind IN ('image', 'source') AND image_artifact_id IS NOT NULL));

-- +goose Down
ALTER TABLE app_revisions DROP CONSTRAINT app_revisions_image_owner_fk;
ALTER TABLE app_revisions DROP CONSTRAINT app_revisions_source_artifact_ck;
ALTER TABLE app_revisions DROP COLUMN image_artifact_id;
ALTER TABLE app_revisions DROP COLUMN source_kind;
DROP TABLE image_artifacts;
