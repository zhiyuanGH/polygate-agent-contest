package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
	"polygate/internal/server"
)

// QueueRunningResourceUpdate keeps the successful revision and its budget in
// place until the replacement is ready. Recreate reserves the larger of the
// old and new usages; Rolling reserves the whole new usage in addition to old.
func (s *Store) QueueRunningResourceUpdate(ctx context.Context, accountID, subject, appID, key string,
	resources domain.Resources, strategy string) (server.DeploymentTask, bool, error) {
	return s.queueRunningSpecUpdate(ctx, accountID, subject, appID, key, resources, strategy, "Resources")
}

// QueueRunningRestart reuses the durable update and rollback path. Rolling
// requires surge quota; Recreate can restart within the existing reservation.
func (s *Store) QueueRunningRestart(ctx context.Context, accountID, subject, appID, key,
	strategy string) (server.DeploymentTask, bool, error) {
	return s.queueRunningSpecUpdate(ctx, accountID, subject, appID, key, domain.Resources{}, strategy, "Restart")
}

func (s *Store) queueRunningSpecUpdate(ctx context.Context, accountID, subject, appID, key string,
	resources domain.Resources, strategy, updateKind string) (server.DeploymentTask, bool, error) {
	var task server.DeploymentTask
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		return task, false, &domain.Error{Code: "INVALID_IDEMPOTENCY_KEY", Message: "Send an Idempotency-Key of 8–128 non-space characters."}
	}
	if strategy != "Rolling" && strategy != "Recreate" {
		return task, false, &domain.Error{Code: "INVALID_ROLLOUT_STRATEGY", Field: "strategy", Message: "Choose Rolling or Recreate."}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return task, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, volumeEnabled bool
	var quota domain.Quota
	err = tx.QueryRow(ctx, `SELECT enabled,volume_enabled,quota_cpu_request_milli,quota_cpu_limit_milli,
		quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(
		&enabled, &volumeEnabled, &quota.CPURequestMilli, &quota.CPULimitMilli,
		&quota.MemoryMiB, &quota.Pods, &quota.Apps)
	if err != nil {
		return task, false, storeError(err)
	}
	if !enabled {
		return task, false, server.ErrDisabled
	}
	operationKind := "RESOURCE_UPDATE_REQUEST"
	if updateKind == "Restart" {
		operationKind = "RESTART_REQUEST"
	}
	var replayKind, replayUpdateKind, replayStrategy string
	var replaySpec []byte
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.account_id::text,t.app_id::text,t.revision_id::text,
		t.desired_state,t.state,t.created_at,t.task_kind,t.update_kind,t.rollout_strategy,r.spec,
		COALESCE((SELECT o.id::text FROM operations o WHERE o.account_id=t.account_id
		 AND o.app_id=t.app_id AND o.kind=$3 AND o.detail->>'taskId'=t.id::text LIMIT 1),'')
		FROM deployment_tasks t JOIN app_revisions r ON r.id=t.revision_id
		WHERE t.account_id=$1 AND t.idempotency_key=$2`, accountID, key, operationKind).Scan(
		&task.ID, &task.AccountID, &task.AppID, &task.RevisionID, &task.DesiredState,
		&task.State, &task.CreatedAt, &replayKind, &replayUpdateKind, &replayStrategy,
		&replaySpec, &task.OperationID)
	if err == nil {
		var spec domain.AppSpec
		if err := json.Unmarshal(replaySpec, &spec); err != nil {
			return server.DeploymentTask{}, false, err
		}
		if replayKind != "Update" || replayUpdateKind != updateKind || task.AppID != appID ||
			replayStrategy != strategy || (updateKind == "Resources" && spec.Resources != resources) || task.OperationID == "" {
			return server.DeploymentTask{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used for another operation."}
		}
		return task, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return task, false, storeError(err)
	}
	app, err := scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE account_id=$1 AND id=$2 FOR UPDATE`, accountID, appID))
	if err != nil {
		return task, false, storeError(err)
	}
	if app.Status != "Running" {
		return task, false, &domain.Error{Code: "APP_NOT_UPDATABLE", Message: "Only running apps can be updated."}
	}
	if app.Spec.Persistent && strategy != "Recreate" {
		return task, false, &domain.Error{Code: "INVALID_ROLLOUT_STRATEGY", Field: "strategy", Message: "Persistent apps require Recreate."}
	}
	if app.Spec.Persistent {
		if !volumeEnabled {
			return task, false, server.ErrVolumeDisabled
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM student_volumes WHERE id=$1 AND account_id=$2 FOR SHARE`,
			app.Spec.VolumeID, accountID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "Ready") {
			return task, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Wait until the bound volume is ready."}
		}
		if err != nil {
			return task, false, storeError(err)
		}
	}
	var priorRevisionID, priorDigest, sourceKind, artifactID string
	var priorSpecJSON []byte
	var old domain.Usage
	err = tx.QueryRow(ctx, `SELECT rr.revision_id::text,r.platform_image_digest,r.source_kind,
		COALESCE(r.image_artifact_id::text,''),r.spec,
		rr.cpu_request_milli,rr.cpu_limit_milli,rr.memory_mib,rr.pods
		FROM resource_reservations rr JOIN app_revisions r ON r.id=rr.revision_id
		WHERE rr.account_id=$1 AND rr.app_id=$2 FOR UPDATE OF rr`, accountID, appID).Scan(
		&priorRevisionID, &priorDigest, &sourceKind, &artifactID, &priorSpecJSON,
		&old.CPURequestMilli, &old.CPULimitMilli, &old.MemoryMiB, &old.Pods)
	if err != nil {
		return task, false, storeError(err)
	}
	var priorSpec domain.AppSpec
	if err := json.Unmarshal(priorSpecJSON, &priorSpec); err != nil {
		return task, false, err
	}
	if updateKind == "Restart" {
		resources = priorSpec.Resources
	}
	if old != (domain.Usage{CPURequestMilli: priorSpec.Resources.Usage().CPURequestMilli,
		CPULimitMilli: priorSpec.Resources.Usage().CPULimitMilli,
		MemoryMiB:     priorSpec.Resources.Usage().MemoryMiB, Pods: priorSpec.Resources.Replicas}) ||
		app.Spec.Resources != priorSpec.Resources {
		return task, false, &domain.Error{Code: "APP_RESERVATION_MISMATCH", Message: "Resource accounting needs reconciliation before this update."}
	}
	if sourceKind != "image" && sourceKind != "trusted" || kubeobjects.ValidatePlatformDigest(priorDigest) != nil {
		return task, false, &domain.Error{Code: "APP_NOT_UPDATABLE", Message: "This running image cannot be updated."}
	}
	newSpec := priorSpec
	newSpec.Resources = resources
	if err := domain.ValidateSpec(newSpec); err != nil {
		return task, false, err
	}
	if updateKind == "Resources" && resources == priorSpec.Resources {
		return task, false, &domain.Error{Code: "RESOURCES_ALREADY_RUNNING", Message: "Choose different resource settings."}
	}
	if strategy == "Rolling" {
		_, err = kubeobjects.RollingWorkload(accountID, appID, newSpec, priorDigest)
	} else {
		_, err = kubeobjects.TrustedWorkload(accountID, appID, newSpec, priorDigest)
	}
	if err != nil {
		return task, false, err
	}
	var configured domain.Usage
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(cpu_request_milli*replicas),0),
		COALESCE(SUM(cpu_limit_milli*replicas),0),COALESCE(SUM(memory_mib*replicas),0),
		COALESCE(SUM(replicas),0),COUNT(*) FROM apps WHERE account_id=$1`, accountID).Scan(
		&configured.CPURequestMilli, &configured.CPULimitMilli, &configured.MemoryMiB,
		&configured.Pods, &configured.Apps)
	if err != nil {
		return task, false, storeError(err)
	}
	newUsage := resources.Usage()
	if err := checkNoPendingQuotaChange(ctx, tx, accountID); err != nil {
		return task, false, err
	}
	if err := domain.CheckAdmission(configured.Sub(app.Spec.Resources.Usage()), domain.Usage{},
		newUsage, quota, domain.Quota{}); err != nil {
		return task, false, err
	}
	var accountUsed, globalUsed domain.Usage
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(cpu_request_milli),0),COALESCE(SUM(cpu_limit_milli),0),
		COALESCE(SUM(memory_mib),0),COALESCE(SUM(pods),0)
		FROM (SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM resource_reservations WHERE account_id=$1
		UNION ALL SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM rollout_reservations WHERE account_id=$1) u`,
		accountID).Scan(&accountUsed.CPURequestMilli, &accountUsed.CPULimitMilli,
		&accountUsed.MemoryMiB, &accountUsed.Pods)
	if err != nil {
		return task, false, storeError(err)
	}
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations WHERE account_id=$1`, accountID).Scan(&accountUsed.Apps); err != nil {
		return task, false, storeError(err)
	}
	var globalQuota domain.Quota
	err = tx.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods,
		reserved_cpu_request_milli,reserved_cpu_limit_milli,reserved_memory_mib,reserved_pods
		FROM global_budget WHERE id=true FOR UPDATE`).Scan(
		&globalQuota.CPURequestMilli, &globalQuota.CPULimitMilli, &globalQuota.MemoryMiB, &globalQuota.Pods,
		&globalUsed.CPURequestMilli, &globalUsed.CPULimitMilli, &globalUsed.MemoryMiB, &globalUsed.Pods)
	if err != nil {
		return task, false, storeError(err)
	}
	var extra domain.Usage
	var peak domain.Usage
	if strategy == "Rolling" {
		extra = newUsage
	} else {
		peak = maxUsage(old, newUsage)
		extra = peak.Sub(old)
	}
	extra.Apps = 0
	if err := domain.CheckAdmission(accountUsed, globalUsed, extra, quota, globalQuota); err != nil {
		return task, false, err
	}
	var revision int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM app_revisions WHERE app_id=$1`, appID).Scan(&revision); err != nil {
		return task, false, storeError(err)
	}
	revisionID, err := server.NewID()
	if err != nil {
		return task, false, err
	}
	taskID, err := server.NewID()
	if err != nil {
		return task, false, err
	}
	outboxID, err := server.NewID()
	if err != nil {
		return task, false, err
	}
	opID, err := server.NewID()
	if err != nil {
		return task, false, err
	}
	specJSON, err := json.Marshal(newSpec)
	if err != nil {
		return task, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec,source_kind,image_artifact_id)
		VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,NULLIF($8,'')::uuid)`, revisionID, accountID, appID,
		revision, priorDigest, specJSON, sourceKind, artifactID)
	if err != nil {
		return task, false, storeError(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,idempotency_key,
		desired_state,task_kind,prior_revision_id,update_kind,rollout_strategy)
		VALUES($1,$2,$3,$4,$5,'Running','Update',$6,$7,$8) RETURNING created_at`,
		taskID, accountID, appID, revisionID, key, priorRevisionID, updateKind, strategy).Scan(&task.CreatedAt)
	if err != nil {
		return task, false, storeError(err)
	}
	if strategy == "Rolling" {
		_, err = tx.Exec(ctx, `INSERT INTO rollout_reservations(update_task_id,account_id,app_id,
			cpu_request_milli,cpu_limit_milli,memory_mib,pods) VALUES($1,$2,$3,$4,$5,$6,$7)`, taskID,
			accountID, appID, extra.CPURequestMilli, extra.CPULimitMilli, extra.MemoryMiB, extra.Pods)
	} else {
		_, err = tx.Exec(ctx, `UPDATE resource_reservations SET cpu_request_milli=$3,
			cpu_limit_milli=$4,memory_mib=$5,pods=$6 WHERE app_id=$1 AND account_id=$2`,
			appID, accountID, peak.CPURequestMilli, peak.CPULimitMilli, peak.MemoryMiB, peak.Pods)
	}
	if err != nil {
		return task, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE global_budget SET
		reserved_cpu_request_milli=reserved_cpu_request_milli+$1,
		reserved_cpu_limit_milli=reserved_cpu_limit_milli+$2,
		reserved_memory_mib=reserved_memory_mib+$3,reserved_pods=reserved_pods+$4 WHERE id=true`,
		extra.CPURequestMilli, extra.CPULimitMilli, extra.MemoryMiB, extra.Pods)
	if err != nil {
		return task, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE apps SET status='Queued',updated_at=now() WHERE id=$1 AND account_id=$2`, appID, accountID)
	if err != nil {
		return task, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(id,task_id,event_type) VALUES($1,$2,'RECONCILE_DEPLOYMENT')`, outboxID, taskID)
	if err != nil {
		return task, false, storeError(err)
	}
	detail, err := json.Marshal(map[string]any{"appId": appID, "taskId": taskID,
		"revisionId": revisionID, "priorRevisionId": priorRevisionID,
		"strategy": strategy, "before": priorSpec.Resources, "after": resources})
	if err != nil {
		return task, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,$5,'Queued',$6::jsonb)`, opID, accountID, appID, subject, operationKind, detail)
	if err != nil {
		return task, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return task, false, storeError(err)
	}
	return server.DeploymentTask{ID: taskID, OperationID: opID, AccountID: accountID, AppID: appID,
		RevisionID: revisionID, DesiredState: "Running", State: "Queued", CreatedAt: task.CreatedAt}, false, nil
}

func maxUsage(a, b domain.Usage) domain.Usage {
	return domain.Usage{CPURequestMilli: max(a.CPURequestMilli, b.CPURequestMilli),
		CPULimitMilli: max(a.CPULimitMilli, b.CPULimitMilli),
		MemoryMiB:     max(a.MemoryMiB, b.MemoryMiB), Pods: max(a.Pods, b.Pods)}
}

func (s *Store) completeResourceUpdateTx(ctx context.Context, tx pgx.Tx, accountID, appID,
	updateTaskID, revisionID, priorRevisionID, strategy string) error {
	var specJSON []byte
	if err := tx.QueryRow(ctx, `SELECT spec FROM app_revisions WHERE id=$1 AND app_id=$2 AND account_id=$3`,
		revisionID, appID, accountID).Scan(&specJSON); err != nil {
		return storeError(err)
	}
	var spec domain.AppSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return err
	}
	if strategy == "Rolling" {
		if err := releaseRolloutReservation(ctx, tx, updateTaskID); err != nil {
			return err
		}
	}
	if err := setReservationUsageTx(ctx, tx, accountID, appID, priorRevisionID, revisionID, spec.Resources.Usage()); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE apps SET cpu_request_milli=$3,cpu_limit_milli=$4,
		memory_mib=$5,replicas=$6,updated_at=now() WHERE id=$1 AND account_id=$2`,
		appID, accountID, spec.Resources.CPURequestMilli, spec.Resources.CPULimitMilli,
		spec.Resources.MemoryMiB, spec.Resources.Replicas)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("resource update app disappeared")
	}
	return nil
}

func (s *Store) rollbackResourceUpdateTx(ctx context.Context, tx pgx.Tx, accountID, appID,
	failedUpdateTaskID, priorRevisionID, strategy string) error {
	if strategy == "Rolling" {
		return releaseRolloutReservation(ctx, tx, failedUpdateTaskID)
	}
	var specJSON []byte
	if err := tx.QueryRow(ctx, `SELECT spec FROM app_revisions WHERE id=$1 AND app_id=$2 AND account_id=$3`,
		priorRevisionID, appID, accountID).Scan(&specJSON); err != nil {
		return storeError(err)
	}
	var prior domain.AppSpec
	if err := json.Unmarshal(specJSON, &prior); err != nil {
		return err
	}
	return setReservationUsageTx(ctx, tx, accountID, appID, priorRevisionID, priorRevisionID,
		prior.Resources.Usage())
}

func setReservationUsageTx(ctx context.Context, tx pgx.Tx, accountID, appID,
	expectedRevisionID, newRevisionID string, desired domain.Usage) error {
	var current domain.Usage
	if err := tx.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods
		FROM resource_reservations WHERE app_id=$1 AND account_id=$2 AND revision_id=$3 FOR UPDATE`,
		appID, accountID, expectedRevisionID).Scan(&current.CPURequestMilli,
		&current.CPULimitMilli, &current.MemoryMiB, &current.Pods); err != nil {
		return storeError(err)
	}
	delta := desired.Sub(current)
	result, err := tx.Exec(ctx, `UPDATE global_budget SET
		reserved_cpu_request_milli=reserved_cpu_request_milli+$1::bigint,
		reserved_cpu_limit_milli=reserved_cpu_limit_milli+$2::bigint,
		reserved_memory_mib=reserved_memory_mib+$3::bigint,reserved_pods=reserved_pods+$4::bigint
		WHERE id=true AND reserved_cpu_request_milli>=GREATEST(-($1::bigint),0)
		AND reserved_cpu_limit_milli>=GREATEST(-($2::bigint),0)
		AND reserved_memory_mib>=GREATEST(-($3::bigint),0)
		AND reserved_pods>=GREATEST(-($4::bigint),0)`,
		delta.CPURequestMilli, delta.CPULimitMilli, delta.MemoryMiB, delta.Pods)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("global budget does not cover resource adjustment")
	}
	result, err = tx.Exec(ctx, `UPDATE resource_reservations SET revision_id=$4,
		cpu_request_milli=$5,cpu_limit_milli=$6,memory_mib=$7,pods=$8
		WHERE app_id=$1 AND account_id=$2 AND revision_id=$3`, appID, accountID,
		expectedRevisionID, newRevisionID, desired.CPURequestMilli, desired.CPULimitMilli,
		desired.MemoryMiB, desired.Pods)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("resource reservation changed during update")
	}
	return nil
}
