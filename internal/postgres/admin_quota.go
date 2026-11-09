package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/server"
)

const quotaChangeColumns = `id::text,actor_account_id::text,target_account_id::text,
    quota_cpu_request_milli,quota_cpu_limit_milli,quota_memory_mib,quota_pods,quota_apps,
    state,reason,last_error,created_at,applied_at`

func scanQuotaChange(row rowScanner) (server.QuotaChange, error) {
	var item server.QuotaChange
	err := row.Scan(&item.ID, &item.ActorAccountID, &item.TargetAccountID,
		&item.Quota.CPURequestMilli, &item.Quota.CPULimitMilli, &item.Quota.MemoryMiB,
		&item.Quota.Pods, &item.Quota.Apps, &item.State, &item.Reason,
		&item.LastError, &item.CreatedAt, &item.AppliedAt)
	return item, err
}

func (s *Store) QueueAdminQuotaChange(ctx context.Context, actorID, actorSubject, targetID, key, reason string,
	quota domain.Quota) (server.QuotaChange, bool, error) {
	var empty server.QuotaChange
	if actorID == targetID || actorSubject == "" {
		return empty, false, server.ErrForbidden
	}
	if err := domain.ValidateAccountQuota(quota); err != nil {
		return empty, false, err
	}
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") ||
		len(strings.TrimSpace(reason)) < 3 || len(reason) > 2000 {
		return empty, false, &domain.Error{Code: "INVALID_REQUEST", Message: "A valid key and reason are required."}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return empty, false, storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var actorAllowed bool
	err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&actorAllowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !actorAllowed {
		return empty, false, server.ErrForbidden
	}
	if err != nil {
		return empty, false, storeError(err)
	}
	old, err := scanQuotaChange(tx.QueryRow(ctx, `SELECT `+quotaChangeColumns+` FROM account_quota_changes
        WHERE actor_account_id=$1 AND idempotency_key=$2`, actorID, key))
	if err == nil {
		if old.TargetAccountID != targetID || old.Quota != quota || old.Reason != reason {
			return empty, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used."}
		}
		return old, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, storeError(err)
	}
	var targetRole string
	var current domain.Quota
	err = tx.QueryRow(ctx, `SELECT role,quota_cpu_request_milli,quota_cpu_limit_milli,
        quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, targetID).Scan(
		&targetRole, &current.CPURequestMilli, &current.CPULimitMilli,
		&current.MemoryMiB, &current.Pods, &current.Apps)
	if err != nil {
		return empty, false, storeError(err)
	}
	if targetRole != "student" {
		return empty, false, server.ErrForbidden
	}
	if current == quota {
		return empty, false, &domain.Error{Code: "QUOTA_UNCHANGED", Message: "The account already has this quota."}
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_quota_changes
        WHERE target_account_id=$1 AND state='Queued')`, targetID).Scan(&pending)
	if err != nil {
		return empty, false, storeError(err)
	}
	if pending {
		return empty, false, &domain.Error{Code: "QUOTA_CHANGE_PENDING", Message: "An account quota change is already pending."}
	}
	id, err := server.NewID()
	if err != nil {
		return empty, false, err
	}
	item, err := scanQuotaChange(tx.QueryRow(ctx, `INSERT INTO account_quota_changes
        (id,actor_account_id,target_account_id,actor_subject,idempotency_key,reason,
         quota_cpu_request_milli,quota_cpu_limit_milli,quota_memory_mib,quota_pods,quota_apps)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+quotaChangeColumns,
		id, actorID, targetID, actorSubject, key, reason,
		quota.CPURequestMilli, quota.CPULimitMilli, quota.MemoryMiB, quota.Pods, quota.Apps))
	if err != nil {
		return empty, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, false, storeError(err)
	}
	return item, false, nil
}

func (s *Store) ListAdminQuotaChanges(ctx context.Context, actorID string) ([]server.QuotaChange, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+quotaChangeColumns+` FROM account_quota_changes
        ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.QuotaChange, 0)
	for rows.Next() {
		item, err := scanQuotaChange(rows)
		if err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}

func checkNoPendingQuotaChange(ctx context.Context, tx pgx.Tx, accountID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_quota_changes
        WHERE target_account_id=$1 AND state='Queued')`, accountID).Scan(&pending); err != nil {
		return storeError(err)
	}
	if pending {
		return &domain.Error{Code: "QUOTA_CHANGE_PENDING",
			Message: "Account quota is being updated. Retry after the change is applied."}
	}
	return nil
}

// ProcessNextQuotaChange holds the account row lock across the Kubernetes
// patch and database commit. A crash after the patch leaves a queued request;
// the next run reapplies the same desired values before committing the account.
func (s *Store) ProcessNextQuotaChange(ctx context.Context,
	apply func(context.Context, string, domain.Quota) error) (bool, error) {
	var targetID, changeID string
	err := s.Pool.QueryRow(ctx, `SELECT id::text,target_account_id::text FROM account_quota_changes
        WHERE state='Queued' ORDER BY created_at,id LIMIT 1`).Scan(&changeID, &targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, storeError(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var targetRole string
	err = tx.QueryRow(ctx, `SELECT role FROM accounts WHERE id=$1 FOR UPDATE`, targetID).Scan(&targetRole)
	if err != nil {
		return false, storeError(err)
	}
	if targetRole != "student" {
		return false, server.ErrForbidden
	}
	item, err := scanQuotaChange(tx.QueryRow(ctx, `SELECT `+quotaChangeColumns+` FROM account_quota_changes
        WHERE id=$1 AND state='Queued' FOR UPDATE SKIP LOCKED`, changeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, storeError(err)
	}
	if item.TargetAccountID != targetID {
		return false, errors.New("quota target changed unexpectedly")
	}
	if err = apply(ctx, targetID, item.Quota); err != nil {
		_ = tx.Rollback(ctx)
		_, _ = s.Pool.Exec(ctx, `UPDATE account_quota_changes SET attempt=attempt+1,
            last_error='KUBERNETES_QUOTA_APPLY_FAILED' WHERE id=$1 AND state='Queued'`, changeID)
		return true, err
	}
	_, err = tx.Exec(ctx, `UPDATE accounts SET quota_cpu_request_milli=$2,quota_cpu_limit_milli=$3,
        quota_memory_mib=$4,quota_pods=$5,quota_apps=$6 WHERE id=$1`, targetID,
		item.Quota.CPURequestMilli, item.Quota.CPULimitMilli, item.Quota.MemoryMiB,
		item.Quota.Pods, item.Quota.Apps)
	if err != nil {
		return true, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE account_quota_changes SET state='Applied',applied_at=now(),
        attempt=attempt+1,last_error='' WHERE id=$1`, changeID)
	if err != nil {
		return true, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return true, storeError(err)
	}
	return true, nil
}

type AccountQuotaTarget struct {
	AccountID string
	Quota     domain.Quota
}

// Pending changes are excluded so the periodic drift repair never reverts an
// in-flight Kubernetes patch to the previous database value.
func (s *Store) ListAccountQuotaTargets(ctx context.Context) ([]AccountQuotaTarget, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.id::text,a.quota_cpu_request_milli,a.quota_cpu_limit_milli,
        a.quota_memory_mib,a.quota_pods,a.quota_apps FROM accounts a
        WHERE a.role='student' AND NOT EXISTS(
            SELECT 1 FROM account_quota_changes c WHERE c.target_account_id=a.id AND c.state='Queued')
        ORDER BY a.id LIMIT 1000`)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]AccountQuotaTarget, 0)
	for rows.Next() {
		var item AccountQuotaTarget
		if err := rows.Scan(&item.AccountID, &item.Quota.CPURequestMilli,
			&item.Quota.CPULimitMilli, &item.Quota.MemoryMiB,
			&item.Quota.Pods, &item.Quota.Apps); err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}
