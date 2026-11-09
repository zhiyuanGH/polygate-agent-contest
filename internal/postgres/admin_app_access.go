package postgres

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/server"
)

const adminAppColumns = `app.id::text,app.account_id::text,owner.username,app.name,
	app.status,app.access_mode,app.created_at`

func scanAdminApp(row rowScanner) (server.AdminApp, error) {
	var app server.AdminApp
	err := row.Scan(&app.ID, &app.OwnerID, &app.Username, &app.Name,
		&app.Status, &app.AccessMode, &app.CreatedAt)
	return app, err
}

func (s *Store) ListAdminApps(ctx context.Context, actorID string) ([]server.AdminApp, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+adminAppColumns+` FROM apps app
		JOIN accounts owner ON owner.id=app.account_id ORDER BY owner.username,app.name,app.id LIMIT 1000`)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	apps := make([]server.AdminApp, 0)
	for rows.Next() {
		app, err := scanAdminApp(rows)
		if err != nil {
			return nil, storeError(err)
		}
		apps = append(apps, app)
	}
	return apps, storeError(rows.Err())
}

func (s *Store) SetAdminAppAccess(ctx context.Context, actorID, actorSubject, appID, mode, reason string) (server.AdminApp, error) {
	var empty server.AdminApp
	reason = strings.TrimSpace(reason)
	if (mode != AppAccessPublic && mode != AppAccessPrivate) || actorSubject == "" ||
		utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 500 {
		return empty, &domain.Error{Code: "INVALID_ACCESS_CHANGE", Message: "Choose an access mode and enter a reason of 3 to 500 characters."}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, storeError(err)
	}
	defer tx.Rollback(ctx)
	var adminAllowed bool
	err = tx.QueryRow(ctx, `SELECT role='admin' AND enabled FROM accounts WHERE id=$1 FOR SHARE`, actorID).Scan(&adminAllowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !adminAllowed {
		return empty, server.ErrForbidden
	}
	if err != nil {
		return empty, storeError(err)
	}
	app, err := scanAdminApp(tx.QueryRow(ctx, `SELECT `+adminAppColumns+` FROM apps app
		JOIN accounts owner ON owner.id=app.account_id WHERE app.id=$1 FOR UPDATE OF app`, appID))
	if err != nil {
		return empty, storeError(err)
	}
	if app.AccessMode == mode {
		return app, nil
	}
	if mode == AppAccessPrivate && app.Status != "Running" {
		return empty, &domain.Error{Code: "APP_NOT_RUNNING", Message: "Deploy and start the app before restricting access."}
	}
	oldMode := app.AccessMode
	if _, err = tx.Exec(ctx, `UPDATE apps SET access_mode=$2,updated_at=now() WHERE id=$1`, appID, mode); err != nil {
		return empty, storeError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM app_access_tickets WHERE app_id=$1`, appID); err != nil {
		return empty, storeError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM app_access_sessions WHERE app_id=$1`, appID); err != nil {
		return empty, storeError(err)
	}
	auditID, err := server.NewID()
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_app_access_audit
		(id,actor_account_id,actor_subject,owner_account_id,app_id,app_name,previous_mode,new_mode,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, auditID, actorID, actorSubject, app.OwnerID,
		appID, app.Name, oldMode, mode, reason); err != nil {
		return empty, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	app.AccessMode = mode
	return app, nil
}

func (s *Store) ListAdminAppAccessAudit(ctx context.Context, actorID string, limit int) ([]server.AdminAppAccessAudit, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT audit.id::text,audit.actor_subject,
		COALESCE(owner.username,''),audit.app_id::text,audit.app_name,audit.previous_mode,
		audit.new_mode,audit.reason,audit.created_at
		FROM admin_app_access_audit audit LEFT JOIN accounts owner ON owner.id=audit.owner_account_id
		ORDER BY audit.created_at DESC,audit.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	result := make([]server.AdminAppAccessAudit, 0)
	for rows.Next() {
		var item server.AdminAppAccessAudit
		if err := rows.Scan(&item.ID, &item.ActorSubject, &item.Username, &item.AppID,
			&item.AppName, &item.PreviousMode, &item.NewMode, &item.Reason, &item.CreatedAt); err != nil {
			return nil, storeError(err)
		}
		result = append(result, item)
	}
	return result, storeError(rows.Err())
}
