package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"polygate/internal/deploy"
	"polygate/internal/server"
)

// ClaimNext uses a database lease. Expired work can be claimed again after a
// worker crash; the Kubernetes adapter must therefore apply objects
// idempotently and verify their current state before completion.
func (s *Store) ClaimNext(ctx context.Context, workerID string, lease time.Duration) (deploy.Task, bool, error) {
	var task deploy.Task
	if workerID == "" || lease < 30*time.Second {
		return task, false, errors.New("invalid worker identity or lease")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return task, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var specJSON []byte
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.task_kind,t.update_kind,t.rollout_strategy,t.desired_state,t.account_id::text,t.app_id::text,t.revision_id::text,
	        r.spec,r.platform_image_digest,t.attempt,COALESCE(v.size_gib,0),a.username,
        a.quota_cpu_request_milli,a.quota_cpu_limit_milli,a.quota_memory_mib,a.quota_pods,a.quota_apps
        FROM deployment_tasks t
        JOIN app_revisions r ON r.id=t.revision_id
        JOIN accounts a ON a.id=t.account_id
		JOIN apps ap ON ap.id=t.app_id
		LEFT JOIN student_volumes v ON v.id=ap.volume_id AND v.account_id=t.account_id
        WHERE t.state IN ('Queued','Leased')
          AND (t.lease_until IS NULL OR t.lease_until<=now())
        ORDER BY t.created_at,t.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).Scan(
		&task.ID, &task.Kind, &task.UpdateKind, &task.Strategy, &task.DesiredState, &task.AccountID, &task.AppID, &task.RevisionID,
		&specJSON, &task.ImageDigest, &task.Attempt, &task.VolumeSizeGiB, &task.Username,
		&task.Quota.CPURequestMilli, &task.Quota.CPULimitMilli,
		&task.Quota.MemoryMiB, &task.Quota.Pods, &task.Quota.Apps)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.Task{}, false, nil
	}
	if err != nil {
		return deploy.Task{}, false, storeError(err)
	}
	if err = json.Unmarshal(specJSON, &task.Spec); err != nil {
		return deploy.Task{}, false, err
	}
	err = tx.QueryRow(ctx, `UPDATE deployment_tasks SET state='Leased',locked_by=$2,
        lease_until=now()+($3::bigint * interval '1 millisecond'),
        attempt=attempt+1,updated_at=now()
        WHERE id=$1 RETURNING attempt`, task.ID, workerID, lease.Milliseconds()).Scan(&task.Attempt)
	if err != nil {
		return deploy.Task{}, false, storeError(err)
	}
	status := "Deploying"
	if task.DesiredState == "Stopped" {
		status = "Stopping"
	}
	_, err = tx.Exec(ctx, `UPDATE apps SET status=$3,updated_at=now()
        WHERE id=$1 AND account_id=$2`, task.AppID, task.AccountID, status)
	if err != nil {
		return deploy.Task{}, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return deploy.Task{}, false, storeError(err)
	}
	return task, true, nil
}

func (s *Store) Retry(ctx context.Context, taskID, workerID, errorCode string, delay time.Duration) error {
	if delay < 0 || delay > time.Hour || len(errorCode) > 64 {
		return errors.New("invalid retry parameters")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var desired, kind, updateKind, strategy, accountID, appID, revisionID string
	var priorRevisionID *string
	var attempt int
	var createdAt time.Time
	err = tx.QueryRow(ctx, `SELECT desired_state,task_kind,update_kind,rollout_strategy,account_id::text,app_id::text,
		revision_id::text,prior_revision_id::text,attempt,created_at FROM deployment_tasks
        WHERE id=$1 AND state='Leased' AND locked_by=$2 AND lease_until>now()
        FOR UPDATE`, taskID, workerID).Scan(
		&desired, &kind, &updateKind, &strategy, &accountID, &appID, &revisionID, &priorRevisionID, &attempt, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.ErrLeaseLost
	}
	if err != nil {
		return storeError(err)
	}
	if kind == "Update" && (attempt >= 12 || time.Since(createdAt) >= 15*time.Minute) {
		if priorRevisionID == nil {
			return errors.New("update task has no prior revision")
		}
		if _, err = tx.Exec(ctx, `UPDATE deployment_tasks SET state='Failed',locked_by=NULL,
			lease_until=NULL,error_code='UPDATE_TIMEOUT',updated_at=now() WHERE id=$1`, taskID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox SET state='Dispatched',dispatched_at=now()
			WHERE task_id=$1 AND state='Pending'`, taskID); err != nil {
			return storeError(err)
		}
		rollbackID, idErr := server.NewID()
		if idErr != nil {
			return idErr
		}
		outboxID, idErr := server.NewID()
		if idErr != nil {
			return idErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,
			idempotency_key,desired_state,task_kind,rollback_of_task_id,update_kind,rollout_strategy)
			VALUES($1,$2,$3,$4,$5,'Running','Rollback',$6,$7,$8)`, rollbackID,
			accountID, appID, *priorRevisionID, "auto-rollback:"+taskID, taskID, updateKind, strategy); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO outbox(id,task_id,event_type)
			VALUES($1,$2,'RECONCILE_DEPLOYMENT')`, outboxID, rollbackID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='Reverting',
			detail=jsonb_set(detail,'{errorCode}',to_jsonb('UPDATE_TIMEOUT'::text),true)
			WHERE account_id=$1 AND app_id=$2 AND kind IN ('UPDATE_REQUEST','ROLLBACK_REQUEST','RESOURCE_UPDATE_REQUEST','RESTART_REQUEST')
			  AND detail->>'taskId'=$3`, accountID, appID, taskID); err != nil {
			return storeError(err)
		}
		return storeError(tx.Commit(ctx))
	}
	if kind == "Deploy" && desired == "Running" && (attempt >= 12 || time.Since(createdAt) >= 15*time.Minute) {
		if _, err = tx.Exec(ctx, `UPDATE deployment_tasks SET state='Failed',locked_by=NULL,
            lease_until=NULL,error_code='DEPLOYMENT_TIMEOUT',updated_at=now()
            WHERE id=$1`, taskID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox SET state='Dispatched',dispatched_at=now()
            WHERE task_id=$1 AND state='Pending'`, taskID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='Failed',
            detail=jsonb_set(detail,'{errorCode}',to_jsonb('DEPLOYMENT_TIMEOUT'::text),true)
            WHERE account_id=$1 AND app_id=$2 AND kind='DEPLOY_REQUEST'
              AND detail->>'taskId'=$3`, accountID, appID, taskID); err != nil {
			return storeError(err)
		}
		_, err = enqueueCleanupTx(ctx, tx, accountID, appID, revisionID,
			"system:worker", "auto-cleanup:"+taskID, "AUTO_CLEANUP", "Failed", nil)
		if err != nil {
			return err
		}
		return storeError(tx.Commit(ctx))
	}
	_, err = tx.Exec(ctx, `UPDATE deployment_tasks
        SET state='Queued',locked_by=NULL,
            lease_until=now()+($4::bigint * interval '1 millisecond'),
            error_code=$3,updated_at=now()
        WHERE id=$1 AND state='Leased' AND locked_by=$2`,
		taskID, workerID, errorCode, delay.Milliseconds())
	if err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}

func (s *Store) Complete(ctx context.Context, taskID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var appID, accountID, revisionID, kind, updateKind, strategy string
	var rollbackOf *string
	var priorRevisionID *string
	var persistent bool
	err = tx.QueryRow(ctx, `UPDATE deployment_tasks SET state='Succeeded',locked_by=NULL,
        lease_until=NULL,error_code=NULL,updated_at=now()
        WHERE id=$1 AND state='Leased' AND locked_by=$2 AND lease_until>now()
		RETURNING app_id::text,account_id::text,revision_id::text,task_kind,update_kind,rollout_strategy,
		prior_revision_id::text,rollback_of_task_id::text,
		(SELECT (r.spec->>'persistent')::boolean FROM app_revisions r WHERE r.id=deployment_tasks.revision_id)`,
		taskID, workerID).Scan(&appID, &accountID, &revisionID, &kind, &updateKind,
		&strategy, &priorRevisionID, &rollbackOf, &persistent)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.ErrLeaseLost
	}
	if err != nil {
		return storeError(err)
	}
	if kind == "Update" {
		if updateKind == "Resources" || updateKind == "Restart" {
			if priorRevisionID == nil {
				return errors.New("resource update has no prior revision")
			}
			if err := s.completeResourceUpdateTx(ctx, tx, accountID, appID, taskID,
				revisionID, *priorRevisionID, strategy); err != nil {
				return err
			}
		} else {
			result, err := tx.Exec(ctx, `UPDATE resource_reservations SET revision_id=$3
			WHERE app_id=$1 AND account_id=$2 AND revision_id=(
			 SELECT prior_revision_id FROM deployment_tasks WHERE id=$4)`, appID, accountID, revisionID, taskID)
			if err != nil {
				return storeError(err)
			}
			if result.RowsAffected() != 1 {
				return errors.New("prior revision reservation changed during update")
			}
			if !persistent {
				if err := releaseRolloutReservation(ctx, tx, taskID); err != nil {
					return err
				}
			}
		}
	} else if kind == "Rollback" {
		if rollbackOf == nil {
			return errors.New("rollback task has no failed update")
		}
		if updateKind == "Resources" || updateKind == "Restart" {
			if err := s.rollbackResourceUpdateTx(ctx, tx, accountID, appID,
				*rollbackOf, revisionID, strategy); err != nil {
				return err
			}
		} else if !persistent {
			if err := releaseRolloutReservation(ctx, tx, *rollbackOf); err != nil {
				return err
			}
		}
	}
	result, err := tx.Exec(ctx, `UPDATE apps SET status='Running',updated_at=now()
        WHERE id=$1 AND account_id=$2`, appID, accountID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("task app disappeared")
	}
	result, err = tx.Exec(ctx, `UPDATE outbox SET state='Dispatched',dispatched_at=now()
        WHERE task_id=$1 AND state='Pending'`, taskID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("task outbox event missing")
	}
	operationKind, operationState, operationTaskID := "DEPLOY_REQUEST", "Succeeded", taskID
	if kind == "Update" {
		operationKind = "UPDATE_REQUEST"
		if updateKind == "Resources" {
			operationKind = "RESOURCE_UPDATE_REQUEST"
		} else if updateKind == "Restart" {
			operationKind = "RESTART_REQUEST"
		}
	} else if kind == "Rollback" {
		operationKind, operationState, operationTaskID = "UPDATE_REQUEST", "Failed", *rollbackOf
		if updateKind == "Resources" {
			operationKind = "RESOURCE_UPDATE_REQUEST"
		} else if updateKind == "Restart" {
			operationKind = "RESTART_REQUEST"
		}
	}
	result, err = tx.Exec(ctx, `UPDATE operations SET state=$4
		WHERE account_id=$1 AND app_id=$2 AND
		  (kind=$3 OR ($3='UPDATE_REQUEST' AND kind='ROLLBACK_REQUEST'))
		  AND detail->>'taskId'=$5`, accountID, appID, operationKind, operationState, operationTaskID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("task audit operation missing")
	}
	return storeError(tx.Commit(ctx))
}

func releaseRolloutReservation(ctx context.Context, tx pgx.Tx, updateTaskID string) error {
	var cpuRequest, cpuLimit, memory, pods int64
	err := tx.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods
		FROM rollout_reservations WHERE update_task_id=$1 FOR UPDATE`, updateTaskID).Scan(
		&cpuRequest, &cpuLimit, &memory, &pods)
	if err != nil {
		return storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE global_budget SET
		reserved_cpu_request_milli=reserved_cpu_request_milli-$1,
		reserved_cpu_limit_milli=reserved_cpu_limit_milli-$2,
		reserved_memory_mib=reserved_memory_mib-$3,reserved_pods=reserved_pods-$4
		WHERE id=true AND reserved_cpu_request_milli>=$1 AND reserved_cpu_limit_milli>=$2
		AND reserved_memory_mib>=$3 AND reserved_pods>=$4`, cpuRequest, cpuLimit, memory, pods)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("global budget does not cover rollout reservation")
	}
	result, err = tx.Exec(ctx, `DELETE FROM rollout_reservations WHERE update_task_id=$1`, updateTaskID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("rollout reservation disappeared")
	}
	return nil
}

// CompleteStop must only be called after the cluster adapter has observed
// that the Deployment, Service and matching Pods are all gone. It releases
// the exact reserved amounts in the same transaction as the app status.
func (s *Store) CompleteStop(ctx context.Context, taskID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var appID, accountID, revisionID, completionStatus string
	err = tx.QueryRow(ctx, `UPDATE deployment_tasks SET state='Succeeded',locked_by=NULL,
	    lease_until=NULL,error_code=CASE WHEN completion_status='Failed'
	        THEN 'DEPLOYMENT_FAILED' ELSE NULL END,updated_at=now()
        WHERE id=$1 AND desired_state='Stopped' AND state='Leased'
          AND locked_by=$2 AND lease_until>now()
	    RETURNING app_id::text,account_id::text,revision_id::text,completion_status`,
		taskID, workerID).Scan(&appID, &accountID, &revisionID, &completionStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.ErrLeaseLost
	}
	if err != nil {
		return storeError(err)
	}
	var requestCPU, limitCPU, memoryMiB, pods int64
	err = tx.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods
        FROM resource_reservations
        WHERE app_id=$1 AND account_id=$2 AND revision_id=$3 FOR UPDATE`,
		appID, accountID, revisionID).Scan(&requestCPU, &limitCPU, &memoryMiB, &pods)
	if err != nil {
		return storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE global_budget SET
        reserved_cpu_request_milli=reserved_cpu_request_milli-$1,
        reserved_cpu_limit_milli=reserved_cpu_limit_milli-$2,
        reserved_memory_mib=reserved_memory_mib-$3,
        reserved_pods=reserved_pods-$4
        WHERE id=true AND reserved_cpu_request_milli>=$1
          AND reserved_cpu_limit_milli>=$2 AND reserved_memory_mib>=$3
          AND reserved_pods>=$4`, requestCPU, limitCPU, memoryMiB, pods)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("global budget does not cover the reservation")
	}
	result, err = tx.Exec(ctx, `DELETE FROM resource_reservations
        WHERE app_id=$1 AND account_id=$2 AND revision_id=$3`, appID, accountID, revisionID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("reservation disappeared during stop")
	}
	result, err = tx.Exec(ctx, `UPDATE apps SET status=$3,updated_at=now()
        WHERE id=$1 AND account_id=$2 AND status='Stopping'`, appID, accountID, completionStatus)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("app is no longer stopping")
	}
	result, err = tx.Exec(ctx, `UPDATE outbox SET state='Dispatched',dispatched_at=now()
        WHERE task_id=$1 AND state='Pending'`, taskID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("stop outbox event missing")
	}
	result, err = tx.Exec(ctx, `UPDATE operations SET state='Succeeded'
        WHERE account_id=$1 AND app_id=$2 AND kind IN ('STOP_REQUEST','AUTO_CLEANUP')
          AND detail->>'taskId'=$3`, accountID, appID, taskID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("stop audit operation missing")
	}
	return storeError(tx.Commit(ctx))
}
