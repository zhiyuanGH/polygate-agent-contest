-- +goose Up
-- Keep the exact Trivy JSON returned for each completed scan so a student's
-- report remains available after Harbor is unavailable or a scan is replaced.
-- Existing rows are filled by the report backfill worker after deployment.
ALTER TABLE image_artifacts
    ADD COLUMN scan_report_body bytea,
    ADD COLUMN scan_report_body_sha256 text;

ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_report_body_ck CHECK (
    (scan_report_body IS NULL AND scan_report_body_sha256 IS NULL)
    OR
    (scan_status IN ('Approved', 'Rejected') AND
     scan_report_body IS NOT NULL AND octet_length(scan_report_body) BETWEEN 1 AND 8388608 AND
     scan_report_body_sha256 ~ '^sha256:[0-9a-f]{64}$')
);

-- +goose Down
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_report_body_ck;
ALTER TABLE image_artifacts DROP COLUMN scan_report_body_sha256;
ALTER TABLE image_artifacts DROP COLUMN scan_report_body;
