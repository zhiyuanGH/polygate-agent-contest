package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

// enqueueCleanupTx is called while the app row is locked. The caller must
// first make the old deployment task inactive, if one exists. No reservation
// is released until CompleteStop confirms that Kubernetes objects are gone.
func enqueueCleanupTx(ctx context.Context, tx pgx.Tx, accountID, appID, revisionID,
	actor, key, kind, completionStatus string, notBefore *time.Time) (server.DeploymentTask, error) {
	if kind != "STOP_REQUEST" && kind != "AUTO_CLEANUP" {
		return server.DeploymentTask{}, errors.New("invalid cleanup operation kind")
	}
	if completionStatus != "Stopped" && completionStatus != "Failed" {
		return server.DeploymentTask{}, errors.New("invalid cleanup outcome")
	}
	taskID, err := server.NewID()
	if err != nil {
		return server.DeploymentTask{}, err
	}
	opID, err := server.NewID()
	if err != nil {
		return server.DeploymentTask{}, err
	}
	outboxID, err := server.NewID()
	if err != nil {
		return server.DeploymentTask{}, err
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO deployment_tasks
		(id,account_id,app_id,revision_id,idempotency_key,desired_state,task_kind,completion_status,lease_until)
		VALUES($1,$2,$3,$4,$5,'Stopped','Stop',$6,$7) RETURNING created_at`,
		taskID, accountID, appID, revisionID, key, completionStatus, notBefore).Scan(&createdAt)
	if err != nil {
		return server.DeploymentTask{}, storeError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(id,task_id,event_type)
        VALUES($1,$2,'RECONCILE_DEPLOYMENT')`, outboxID, taskID); err != nil {
		return server.DeploymentTask{}, storeError(err)
	}
	detail, err := json.Marshal(map[string]string{"appId": appID, "taskId": taskID, "revisionId": revisionID})
	if err != nil {
		return server.DeploymentTask{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,$4,$5,'Queued',$6::jsonb)`, opID, accountID, appID, actor, kind, detail); err != nil {
		return server.DeploymentTask{}, storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE apps SET status='Stopping',updated_at=now()
        WHERE account_id=$1 AND id=$2`, accountID, appID)
	if err != nil {
		return server.DeploymentTask{}, storeError(err)
	}
	if result.RowsAffected() != 1 {
		return server.DeploymentTask{}, errors.New("cleanup app disappeared")
	}
	return server.DeploymentTask{ID: taskID, OperationID: opID, AccountID: accountID,
		AppID: appID, RevisionID: revisionID, DesiredState: "Stopped", State: "Queued", CreatedAt: createdAt}, nil
}
