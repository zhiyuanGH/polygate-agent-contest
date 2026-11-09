package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// QueueStop keeps the existing reservation until the worker verifies that the
// Deployment, Service and matching Pods have disappeared.
func (s *Store) QueueStop(ctx context.Context, accountID, subject, appID, key string) (server.DeploymentTask, bool, error) {
	var task server.DeploymentTask
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		return task, false, &domain.Error{Code: "INVALID_IDEMPOTENCY_KEY", Message: "Send an Idempotency-Key of 8–128 non-space characters."}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return task, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled)
	if err != nil {
		return task, false, storeError(err)
	}
	if !enabled {
		return task, false, server.ErrDisabled
	}
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.account_id::text,t.app_id::text,t.revision_id::text,
        t.desired_state,t.state,t.created_at,
        (SELECT o.id::text FROM operations o WHERE o.account_id=t.account_id
            AND o.app_id=t.app_id AND o.kind='STOP_REQUEST'
            AND o.detail->>'taskId'=t.id::text LIMIT 1)
        FROM deployment_tasks t WHERE t.account_id=$1 AND t.idempotency_key=$2`,
		accountID, key).Scan(&task.ID, &task.AccountID, &task.AppID, &task.RevisionID,
		&task.DesiredState, &task.State, &task.CreatedAt, &task.OperationID)
	if err == nil {
		if task.AppID != appID || task.DesiredState != "Stopped" {
			return server.DeploymentTask{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used for another operation."}
		}
		return task, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return server.DeploymentTask{}, false, storeError(err)
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM apps WHERE account_id=$1 AND id=$2 FOR UPDATE`,
		accountID, appID).Scan(&status)
	if err != nil {
		return server.DeploymentTask{}, false, storeError(err)
	}
	if status != "Running" && status != "Queued" && status != "Deploying" {
		return server.DeploymentTask{}, false, &domain.Error{Code: "APP_NOT_STOPPABLE", Message: "Only running or pending apps can be stopped."}
	}
	err = tx.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations
        WHERE account_id=$1 AND app_id=$2 FOR UPDATE`, accountID, appID).Scan(&task.RevisionID)
	if err != nil {
		return server.DeploymentTask{}, false, storeError(err)
	}
	var notBefore *time.Time
	if status != "Running" {
		var oldID, oldState, oldKind string
		var oldLease *time.Time
		err = tx.QueryRow(ctx, `SELECT id::text,state,task_kind,lease_until FROM deployment_tasks
            WHERE account_id=$1 AND app_id=$2 AND desired_state='Running'
              AND state IN ('Queued','Leased') FOR UPDATE NOWAIT`, accountID, appID).
			Scan(&oldID, &oldState, &oldKind, &oldLease)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				return server.DeploymentTask{}, false, &domain.Error{Code: "TASK_BUSY", Message: "The deployment is changing. Please retry the stop request."}
			}
			return server.DeploymentTask{}, false, storeError(err)
		}
		if oldKind == "Update" || oldKind == "Rollback" {
			return server.DeploymentTask{}, false, &domain.Error{Code: "TASK_BUSY", Message: "Wait until the update or rollback finishes before stopping this app."}
		}
		if oldState == "Leased" && oldLease != nil {
			// The running worker has a 45-second operation deadline and a
			// two-minute lease. Wait beyond that lease before cleanup.
			ready := oldLease.Add(15 * time.Second)
			notBefore = &ready
		}
		if _, err = tx.Exec(ctx, `UPDATE deployment_tasks SET state='Failed',locked_by=NULL,
            lease_until=NULL,error_code='CANCELLED_BY_USER',updated_at=now() WHERE id=$1`, oldID); err != nil {
			return server.DeploymentTask{}, false, storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox SET state='Dispatched',dispatched_at=now()
            WHERE task_id=$1 AND state='Pending'`, oldID); err != nil {
			return server.DeploymentTask{}, false, storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='Cancelled'
            WHERE account_id=$1 AND app_id=$2 AND kind='DEPLOY_REQUEST'
              AND detail->>'taskId'=$3`, accountID, appID, oldID); err != nil {
			return server.DeploymentTask{}, false, storeError(err)
		}
	}
	task, err = enqueueCleanupTx(ctx, tx, accountID, appID, task.RevisionID,
		subject, key, "STOP_REQUEST", "Stopped", notBefore)
	if err != nil {
		return server.DeploymentTask{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return server.DeploymentTask{}, false, storeError(err)
	}
	return task, false, nil
}
