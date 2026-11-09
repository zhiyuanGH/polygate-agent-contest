package postgres

import (
	"context"
	"errors"
)

// ReconcileRevokedSourceBuilds terminates unfinished tasks for accounts whose
// source-build grant or account access was revoked. Both task and audit state
// change in one transaction; terminal tasks retain their ordinary 14-day
// source retention period.
func (s *Store) ReconcileRevokedSourceBuilds(ctx context.Context) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, storeError(err)
	}
	defer tx.Rollback(ctx)
	var tasks, operations int64
	err = tx.QueryRow(ctx, `WITH revoked AS (
		UPDATE source_build_tasks t SET state='Failed',locked_by=NULL,
			lease_until=NULL,error_code='BUILD_NOT_ENABLED',updated_at=now()
		FROM accounts a WHERE a.id=t.account_id
			AND (NOT a.enabled OR NOT a.source_build_enabled)
			AND t.state IN ('Preparing','Queued','Leased')
		RETURNING t.id::text AS task_id,t.account_id
	), audited AS (
		UPDATE operations o SET state='Failed',
			detail=detail || jsonb_build_object('errorCode','BUILD_NOT_ENABLED')
		FROM revoked r WHERE o.account_id=r.account_id AND o.kind='BUILD_SOURCE'
			AND o.detail->>'taskId'=r.task_id
		RETURNING o.id
	)
	SELECT (SELECT count(*) FROM revoked),(SELECT count(*) FROM audited)`).Scan(&tasks, &operations)
	if err != nil {
		return 0, storeError(err)
	}
	if tasks != operations {
		return 0, errors.New("revoked source build task is missing its audit operation")
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, storeError(err)
	}
	return tasks, nil
}
