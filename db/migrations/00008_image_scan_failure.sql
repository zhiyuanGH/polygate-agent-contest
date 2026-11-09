-- +goose Up
-- A terminal queue failure must also be visible on the artifact and its
-- inspection operation. Failed artifacts have no scan approval evidence.
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_scan_status_check;
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_check;
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_scan_evidence_ck;
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_scan_status_ck
    CHECK (scan_status IN ('PendingScan', 'Approved', 'Rejected', 'Failed'));
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_scan_evidence_ck
    CHECK (
        (scan_status IN ('PendingScan', 'Failed') AND harbor_reference IS NULL AND
         scan_report_sha256 IS NULL AND scan_report_id IS NULL AND
         scanner_name IS NULL AND scan_completed_at IS NULL AND scan_summary IS NULL)
        OR
        (scan_status IN ('Approved', 'Rejected') AND harbor_reference IS NOT NULL AND
         scan_report_sha256 ~ '^sha256:[0-9a-f]{64}$' AND
         scan_report_id IS NOT NULL AND scanner_name IS NOT NULL AND
         scan_completed_at IS NOT NULL AND scan_summary IS NOT NULL)
    );
UPDATE image_artifacts a SET scan_status='Failed'
    FROM image_scan_tasks t WHERE t.artifact_id=a.id AND t.account_id=a.account_id
    AND t.state='Failed' AND a.scan_status='PendingScan';
UPDATE operations o SET state='Failed',
    detail=jsonb_set(o.detail, '{errorCode}', to_jsonb(t.error_code), true)
    FROM image_scan_tasks t WHERE o.account_id=t.account_id
    AND o.kind='IMAGE_INSPECT' AND o.state='PendingScan'
    AND o.detail->>'artifactId'=t.artifact_id::text
    AND t.state='Failed' AND t.error_code IS NOT NULL;

-- +goose Down
UPDATE image_artifacts SET scan_status='PendingScan' WHERE scan_status='Failed';
UPDATE operations SET state='PendingScan', detail=detail-'errorCode'
    WHERE kind='IMAGE_INSPECT' AND state='Failed';
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_scan_status_ck;
ALTER TABLE image_artifacts DROP CONSTRAINT image_artifacts_scan_evidence_ck;
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_scan_status_check
    CHECK (scan_status IN ('PendingScan', 'Approved', 'Rejected'));
ALTER TABLE image_artifacts ADD CONSTRAINT image_artifacts_check
    CHECK (scan_status = 'PendingScan' OR scan_report_sha256 ~ '^sha256:[0-9a-f]{64}$');
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
