package postgres

import (
	"context"
	"encoding/json"

	"polygate/internal/domain"
	"polygate/internal/server"
)

// UpdateAppRuntime repairs an app's saved listening port or health path only
// after its workload is no longer active. Deployment uses the new saved spec.
func (s *Store) UpdateAppRuntime(ctx context.Context, accountID, subject, appID string,
	port int, healthPath string) (server.App, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return server.App{}, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled); err != nil {
		return server.App{}, storeError(err)
	}
	if !enabled {
		return server.App{}, server.ErrDisabled
	}
	app, err := scanApp(tx.QueryRow(ctx,
		`SELECT `+appColumns+` FROM apps WHERE id=$1 AND account_id=$2 FOR UPDATE`, appID, accountID))
	if err != nil {
		return server.App{}, storeError(err)
	}
	if app.Status != "Draft" && app.Status != "Stopped" && app.Status != "Failed" {
		return server.App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its port or health path."}
	}
	updated := app.Spec
	updated.Port = port
	updated.HealthPath = healthPath
	if err = domain.ValidateSpec(updated); err != nil {
		return server.App{}, err
	}
	if app.Spec.Port == port && app.Spec.HealthPath == healthPath {
		return app, nil
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_tasks WHERE app_id=$1
		AND state IN ('Queued','Leased'))`, appID).Scan(&active); err != nil {
		return server.App{}, storeError(err)
	}
	if active {
		return server.App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Wait for the current deployment task to finish."}
	}
	before := map[string]any{"port": app.Spec.Port, "healthPath": app.Spec.HealthPath}
	app, err = scanApp(tx.QueryRow(ctx, `UPDATE apps SET port=$3,health_path=$4,updated_at=now()
		WHERE id=$1 AND account_id=$2 RETURNING `+appColumns,
		appID, accountID, port, healthPath))
	if err != nil {
		return server.App{}, storeError(err)
	}
	detail, err := json.Marshal(map[string]any{"before": before,
		"after": map[string]any{"port": port, "healthPath": healthPath}})
	if err != nil {
		return server.App{}, err
	}
	opID, err := server.NewID()
	if err != nil {
		return server.App{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,'UPDATE_APP_RUNTIME','Succeeded',$5::jsonb)`,
		opID, accountID, appID, subject, detail); err != nil {
		return server.App{}, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return server.App{}, storeError(err)
	}
	return app, nil
}
