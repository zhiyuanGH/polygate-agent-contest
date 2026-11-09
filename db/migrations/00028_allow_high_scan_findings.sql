-- +goose Up
-- Trusted-pilot policy severity-v2 records High findings but allows deploy.
-- Critical and Unknown findings continue to block. Keep the original v1
-- IMAGE_SCAN audit entry; add a separate policy re-evaluation operation.
WITH eligible AS (
    SELECT id, account_id, scan_status AS old_status
    FROM image_artifacts
    WHERE gc_state='Active' AND scan_status IN ('Approved','Rejected')
      AND scan_summary->>'policyVersion'='severity-v1'
      AND jsonb_typeof(scan_summary->'severities')='object'
      AND NOT EXISTS (
          SELECT 1 FROM jsonb_each_text(scan_summary->'severities') AS s(level,count)
          WHERE lower(s.level) IN ('critical','unknown') AND s.count::integer > 0
      )
    FOR UPDATE
), changed AS (
    UPDATE image_artifacts AS a
    SET scan_status='Approved',
        scan_summary=jsonb_set(a.scan_summary,'{policyVersion}',to_jsonb('severity-v2'::text),true)
    FROM eligible AS e WHERE a.id=e.id AND a.account_id=e.account_id
    RETURNING a.id, a.account_id, e.old_status
)
INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
SELECT gen_random_uuid(),account_id,'system:scan-policy','IMAGE_SCAN_POLICY_REEVALUATED',
       'Succeeded',jsonb_build_object('artifactId',id,'fromPolicy','severity-v1',
                                      'toPolicy','severity-v2','previousStatus',old_status,
                                      'newStatus','Approved')
FROM changed WHERE old_status='Rejected';

-- +goose Down
-- Reverting this policy while v2-approved apps are running needs an explicit
-- deployment and admission plan. An automatic down migration could revoke
-- artifacts without stopping their workloads.
DO $$ BEGIN RAISE EXCEPTION 'severity-v2 policy rollback requires a reviewed runbook'; END $$;
