package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// UpdateAppStorage edits a saved binding after the previous workload and all
// capacity reservations have gone. Volume contents and revision history stay
// intact; a subsequent deployment uses the updated app specification.
func (s *Store) UpdateAppStorage(ctx context.Context, accountID, subject, appID string,
	persistent bool, volumeID, mountPath string) (server.App, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return server.App{}, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, volumeEnabled bool
	// Serialize account/app edits without blocking the foreign-key key-share
	// locks needed by a worker that is finishing its task and writing audit rows.
	// This edit changes neither identity key, so a stronger lock is unnecessary.
	if err = tx.QueryRow(ctx, `SELECT enabled,volume_enabled FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accountID).
		Scan(&enabled, &volumeEnabled); err != nil {
		return server.App{}, storeError(err)
	}
	if !enabled {
		return server.App{}, server.ErrDisabled
	}
	app, err := scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+`
		FROM apps WHERE id=$1 AND account_id=$2 FOR NO KEY UPDATE`, appID, accountID))
	if err != nil {
		return server.App{}, storeError(err)
	}
	if app.Status != "Draft" && app.Status != "Stopped" && app.Status != "Failed" {
		return server.App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its storage settings."}
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM resource_reservations WHERE app_id=$1)
		OR EXISTS(SELECT 1 FROM rollout_reservations WHERE app_id=$1)
		OR EXISTS(SELECT 1 FROM deployment_tasks WHERE app_id=$1 AND state IN ('Queued','Leased'))`, appID).Scan(&busy); err != nil {
		return server.App{}, storeError(err)
	}
	if busy {
		return server.App{}, &domain.Error{Code: "APP_CLEANUP_PENDING", Message: "Wait until workload cleanup and resource release finish before changing storage."}
	}
	if app.Status == "Failed" {
		var cleaned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_tasks
			WHERE app_id=$1 AND desired_state IN ('Stopped','Failed') AND state='Succeeded'
			AND created_at >= COALESCE((SELECT max(created_at) FROM deployment_tasks
				WHERE app_id=$1 AND desired_state='Running'), '-infinity'::timestamptz))`, appID).Scan(&cleaned); err != nil {
			return server.App{}, storeError(err)
		}
		if !cleaned {
			return server.App{}, &domain.Error{Code: "APP_CLEANUP_PENDING", Message: "Wait until workload cleanup is verified before changing storage."}
		}
	}
	updated := app.Spec
	updated.Persistent, updated.VolumeID, updated.MountPath = persistent, volumeID, mountPath
	if err = domain.ValidateSpec(updated); err != nil {
		return server.App{}, err
	}
	if persistent {
		if !volumeEnabled {
			return server.App{}, server.ErrVolumeDisabled
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM student_volumes
			WHERE account_id=$1 AND id=$2 FOR UPDATE`, accountID, volumeID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "Ready") {
			return server.App{}, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Select a ready volume from this account."}
		}
		if err != nil {
			return server.App{}, storeError(err)
		}
		var bound bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps
			WHERE volume_id=$1 AND id<>$2)`, volumeID, appID).Scan(&bound); err != nil {
			return server.App{}, storeError(err)
		}
		if bound {
			return server.App{}, &domain.Error{Code: "VOLUME_ALREADY_BOUND", Field: "volumeId", Message: "This volume is already bound to another app."}
		}
	}
	if app.Spec.Persistent == persistent && app.Spec.VolumeID == volumeID && app.Spec.MountPath == mountPath {
		return app, nil
	}
	before := map[string]any{"persistent": app.Spec.Persistent, "volumeId": app.Spec.VolumeID, "mountPath": app.Spec.MountPath}
	app, err = scanApp(tx.QueryRow(ctx, `UPDATE apps SET persistent=$3,
		volume_id=NULLIF($4,'')::uuid,mount_path=NULLIF($5,''),updated_at=now()
		WHERE id=$1 AND account_id=$2 RETURNING `+appColumns, appID, accountID, persistent, volumeID, mountPath))
	if err != nil {
		return server.App{}, createError(err)
	}
	detail, err := json.Marshal(map[string]any{"before": before,
		"after": map[string]any{"persistent": persistent, "volumeId": volumeID, "mountPath": mountPath}})
	if err != nil {
		return server.App{}, err
	}
	opID, err := server.NewID()
	if err != nil {
		return server.App{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,'UPDATE_APP_STORAGE','Succeeded',$5::jsonb)`, opID, accountID, appID, subject, detail); err != nil {
		return server.App{}, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return server.App{}, storeError(err)
	}
	return app, nil
}
