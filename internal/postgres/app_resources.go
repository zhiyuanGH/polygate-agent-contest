package postgres

import (
	"context"
	"encoding/json"

	"polygate/internal/domain"
	"polygate/internal/server"
)

// UpdateAppResources changes configured resources only when no workload is
// running. The account lock serializes this quota check with app creation.
func (s *Store) UpdateAppResources(ctx context.Context, accountID, subject, appID string, resources domain.Resources) (server.App, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return server.App{}, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	var quota domain.Quota
	err = tx.QueryRow(ctx, `SELECT enabled,quota_cpu_request_milli,quota_cpu_limit_milli,
		quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(
		&enabled, &quota.CPURequestMilli, &quota.CPULimitMilli, &quota.MemoryMiB, &quota.Pods, &quota.Apps)
	if err != nil {
		return server.App{}, storeError(err)
	}
	if !enabled {
		return server.App{}, server.ErrDisabled
	}
	app, err := scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE id=$1 AND account_id=$2 FOR UPDATE`, appID, accountID))
	if err != nil {
		return server.App{}, storeError(err)
	}
	if app.Status != "Draft" && app.Status != "Stopped" {
		return server.App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its resource settings."}
	}
	updatedSpec := app.Spec
	updatedSpec.Resources = resources
	if err := domain.ValidateSpec(updatedSpec); err != nil {
		return server.App{}, err
	}
	if app.Spec.Resources == resources {
		return app, nil
	}
	var used domain.Usage
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(cpu_request_milli*replicas),0),
		COALESCE(SUM(cpu_limit_milli*replicas),0),COALESCE(SUM(memory_mib*replicas),0),
		COALESCE(SUM(replicas),0),COUNT(*) FROM apps WHERE account_id=$1`, accountID).Scan(
		&used.CPURequestMilli, &used.CPULimitMilli, &used.MemoryMiB, &used.Pods, &used.Apps)
	if err != nil {
		return server.App{}, storeError(err)
	}
	if err := checkNoPendingQuotaChange(ctx, tx, accountID); err != nil {
		return server.App{}, err
	}
	if err := domain.CheckAdmission(used.Sub(app.Spec.Resources.Usage()), domain.Usage{},
		resources.Usage(), quota, domain.Quota{}); err != nil {
		return server.App{}, err
	}
	before := app.Spec.Resources
	app, err = scanApp(tx.QueryRow(ctx, `UPDATE apps SET cpu_request_milli=$3,cpu_limit_milli=$4,
		memory_mib=$5,replicas=$6,updated_at=now() WHERE id=$1 AND account_id=$2
		RETURNING `+appColumns, appID, accountID, resources.CPURequestMilli,
		resources.CPULimitMilli, resources.MemoryMiB, resources.Replicas))
	if err != nil {
		return server.App{}, storeError(err)
	}
	opID, err := server.NewID()
	if err != nil {
		return server.App{}, err
	}
	detail, err := json.Marshal(map[string]any{"before": before, "after": resources})
	if err != nil {
		return server.App{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,'UPDATE_APP_RESOURCES','Succeeded',$5::jsonb)`, opID, accountID, appID, subject, detail)
	if err != nil {
		return server.App{}, storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return server.App{}, storeError(err)
	}
	return app, nil
}
