package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

const adminAccountColumns = `a.id::text,a.username,a.display_name,a.role,a.enabled,
    a.quota_cpu_request_milli,a.quota_cpu_limit_milli,a.quota_memory_mib,a.quota_pods,a.quota_apps,
    a.image_deploy_enabled,a.source_build_enabled,a.volume_enabled,
    (SELECT count(*)::integer FROM apps WHERE account_id=a.id),a.created_at`

func scanAdminAccount(row rowScanner) (server.AdminAccount, error) {
	var account server.AdminAccount
	err := row.Scan(&account.ID, &account.Username, &account.DisplayName, &account.Role,
		&account.Enabled, &account.Quota.CPURequestMilli, &account.Quota.CPULimitMilli,
		&account.Quota.MemoryMiB, &account.Quota.Pods, &account.Quota.Apps,
		&account.ImageDeployEnabled, &account.SourceBuildEnabled, &account.VolumeEnabled,
		&account.AppCount, &account.CreatedAt)
	return account, err
}

func (s *Store) requireAdmin(ctx context.Context, accountID string) error {
	var allowed bool
	err := s.Pool.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1`, accountID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrForbidden
	}
	if err != nil {
		return storeError(err)
	}
	if !allowed {
		return server.ErrForbidden
	}
	return nil
}

func (s *Store) ListAdminAccounts(ctx context.Context, actorID string) ([]server.AdminAccount, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+adminAccountColumns+` FROM accounts a
        WHERE a.provisioning_state='Active' ORDER BY a.username,a.id LIMIT 1000`)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.AdminAccount, 0)
	for rows.Next() {
		item, err := scanAdminAccount(rows)
		if err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}

func (s *Store) SetAdminAccountEnabled(ctx context.Context, actorID, actorSubject, targetID string,
	enabled bool, reason string) (server.AdminAccount, error) {
	var empty server.AdminAccount
	if actorID == targetID || actorSubject == "" {
		return empty, server.ErrForbidden
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var actorAllowed bool
	err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&actorAllowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !actorAllowed {
		return empty, server.ErrForbidden
	}
	if err != nil {
		return empty, storeError(err)
	}
	var targetRole, provisioningState string
	var oldEnabled bool
	err = tx.QueryRow(ctx, `SELECT role,enabled,provisioning_state FROM accounts WHERE id=$1 FOR UPDATE`, targetID).Scan(&targetRole, &oldEnabled, &provisioningState)
	if err != nil {
		return empty, storeError(err)
	}
	if targetRole != "student" || provisioningState != "Active" {
		return empty, server.ErrForbidden
	}
	if oldEnabled != enabled {
		if _, err = tx.Exec(ctx, `UPDATE accounts SET enabled=$2 WHERE id=$1`, targetID, enabled); err != nil {
			return empty, storeError(err)
		}
		if !enabled {
			if _, err = tx.Exec(ctx, `DELETE FROM auth_sessions WHERE account_id=$1`, targetID); err != nil {
				return empty, storeError(err)
			}
		}
		id, idErr := server.NewID()
		if idErr != nil {
			return empty, idErr
		}
		action := "DISABLE_ACCOUNT"
		if enabled {
			action = "ENABLE_ACCOUNT"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO admin_account_audit
            (id,actor_account_id,target_account_id,actor_subject,action,reason)
            VALUES($1,$2,$3,$4,$5,$6)`, id, actorID, targetID, actorSubject, action, reason); err != nil {
			return empty, storeError(err)
		}
	}
	account, err := scanAdminAccount(tx.QueryRow(ctx, `SELECT `+adminAccountColumns+` FROM accounts a WHERE a.id=$1`, targetID))
	if err != nil {
		return empty, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	return account, nil
}

func (s *Store) ListAdminAccountAudit(ctx context.Context, actorID string, limit int) ([]server.AdminAccountAudit, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT audit.id::text,audit.actor_account_id::text,
        actor.username,audit.target_account_id::text,audit.action,audit.reason,audit.created_at
        FROM admin_account_audit audit JOIN accounts actor ON actor.id=audit.actor_account_id
        ORDER BY audit.created_at DESC,audit.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.AdminAccountAudit, 0)
	for rows.Next() {
		var item server.AdminAccountAudit
		if err := rows.Scan(&item.ID, &item.ActorAccountID, &item.ActorUsername, &item.TargetAccountID,
			&item.Action, &item.Reason, &item.CreatedAt); err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}
