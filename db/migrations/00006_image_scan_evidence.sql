-- +goose Up
-- A completed scan is tied to an immutable Harbor digest and immutable report
-- hash. Pending inspections carry no approval evidence.
ALTER TABLE image_artifacts
    ADD COLUMN harbor_reference text CHECK (harbor_reference IS NULL OR
        (length(harbor_reference) BETWEEN 1 AND 600 AND harbor_reference LIKE '%@sha256:%')),
    ADD COLUMN scan_report_id text CHECK (scan_report_id IS NULL OR
        length(scan_report_id) BETWEEN 1 AND 128),
    ADD COLUMN scanner_name text CHECK (scanner_name IS NULL OR
        length(scanner_name) BETWEEN 1 AND 128),
    ADD COLUMN scan_completed_at timestamptz,
    ADD COLUMN scan_summary jsonb CHECK (scan_summary IS NULL OR
        jsonb_typeof(scan_summary) = 'object');

ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_scan_evidence_ck
    CHECK (
        (scan_status = 'PendingScan' AND harbor_reference IS NULL AND
         scan_report_sha256 IS NULL AND scan_report_id IS NULL AND
         scanner_name IS NULL AND scan_completed_at IS NULL AND scan_summary IS NULL)
        OR
        (scan_status IN ('Approved', 'Rejected') AND harbor_reference IS NOT NULL AND
         scan_report_sha256 IS NOT NULL AND scan_report_id IS NOT NULL AND
         scanner_name IS NOT NULL AND scan_completed_at IS NOT NULL AND
         scan_summary IS NOT NULL)
    );

-- +goose Down
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_scan_evidence_ck;
ALTER TABLE image_artifacts DROP COLUMN scan_summary;
ALTER TABLE image_artifacts DROP COLUMN scan_completed_at;
ALTER TABLE image_artifacts DROP COLUMN scanner_name;
ALTER TABLE image_artifacts DROP COLUMN scan_report_id;
ALTER TABLE image_artifacts DROP COLUMN harbor_reference;
