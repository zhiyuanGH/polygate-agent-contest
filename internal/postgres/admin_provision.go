package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// ClaimAccountSlot leases a preconfigured tenant. A retry after a failed
// Keycloak call reuses the same immutable account ID and resets its password.
func (s *Store) ClaimAccountSlot(ctx context.Context, actorID, username, displayName string) (string, string, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return "", "", err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", "", storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id, state string
	var stale bool
	err = tx.QueryRow(ctx, `SELECT id::text,provisioning_state,
        coalesce(provision_claimed_at < now()-interval '5 minutes',true)
        FROM accounts WHERE username=$1 FOR UPDATE`, username).Scan(&id, &state, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE provisioning_state='Available'
            ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", &domain.Error{Code: "ACCOUNT_CAPACITY_EXHAUSTED", Message: "No prepared account slots are available."}
		}
		if err != nil {
			return "", "", storeError(err)
		}
	} else if err != nil {
		return "", "", storeError(err)
	} else if state != "Creating" || !stale {
		return "", "", &domain.Error{Code: "ACCOUNT_EXISTS_OR_IN_PROGRESS", Field: "username", Message: "This username is already active or being created."}
	}
	claim, err := server.NewID()
	if err != nil {
		return "", "", err
	}
	_, err = tx.Exec(ctx, `UPDATE accounts SET username=$2,display_name=$3,
        provisioning_state='Creating',provision_claim_token=$4,provision_claimed_at=now()
        WHERE id=$1`, id, username, displayName, claim)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", "", &domain.Error{Code: "ACCOUNT_EXISTS_OR_IN_PROGRESS", Field: "username", Message: "This username is already active or being created."}
		}
		return "", "", storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", storeError(err)
	}
	return id, claim, nil
}

func (s *Store) FinishAccountProvision(ctx context.Context, actorID, actorSubject, id, claim, identityID, reason string) (server.AdminAccount, error) {
	var empty server.AdminAccount
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&allowed); err != nil || !allowed {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			return empty, server.ErrForbidden
		}
		return empty, storeError(err)
	}
	command, err := tx.Exec(ctx, `UPDATE accounts SET enabled=true,oidc_subject=$3,provisioning_state='Active',
        provision_claim_token=NULL,provision_claimed_at=NULL
		WHERE id=$1 AND role='student' AND provisioning_state='Creating' AND provision_claim_token=$2`, id, claim, identityID)
	if err != nil {
		return empty, storeError(err)
	}
	if command.RowsAffected() != 1 {
		return empty, &domain.Error{Code: "ACCOUNT_PROVISION_CONFLICT", Message: "The account claim changed; retry provisioning."}
	}
	auditID, err := server.NewID()
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_account_audit
        (id,actor_account_id,target_account_id,actor_subject,action,reason)
        VALUES($1,$2,$3,$4,'CREATE_ACCOUNT',$5)`, auditID, actorID, id, actorSubject, reason); err != nil {
		return empty, storeError(err)
	}
	account, err := scanAdminAccount(tx.QueryRow(ctx, `SELECT `+adminAccountColumns+` FROM accounts a WHERE a.id=$1`, id))
	if err != nil {
		return empty, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	return account, nil
}

func (s *Store) StudentForPasswordReset(ctx context.Context, actorID, targetID string) (string, error) {
	if actorID == targetID {
		return "", server.ErrForbidden
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&allowed); err != nil || !allowed {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			return "", server.ErrForbidden
		}
		return "", storeError(err)
	}
	var subject string
	err = tx.QueryRow(ctx, `SELECT oidc_subject FROM accounts
		WHERE id=$1 AND role='student' AND provisioning_state='Active' FOR UPDATE`, targetID).Scan(&subject)
	if err != nil {
		return "", storeError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM auth_sessions WHERE account_id=$1`, targetID); err != nil {
		return "", storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", storeError(err)
	}
	return subject, nil
}

func (s *Store) RecordPasswordReset(ctx context.Context, actorID, actorSubject, targetID, reason string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&allowed); err != nil || !allowed {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			return server.ErrForbidden
		}
		return storeError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM auth_sessions WHERE account_id=$1`, targetID); err != nil {
		return storeError(err)
	}
	id, err := server.NewID()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_account_audit
        (id,actor_account_id,target_account_id,actor_subject,action,reason)
        VALUES($1,$2,$3,$4,'RESET_PASSWORD',$5)`, id, actorID, targetID, actorSubject, reason); err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}
