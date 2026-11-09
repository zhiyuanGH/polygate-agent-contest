package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/server"
)

// QueueApprovedImageUpdate replaces a Running app with an approved image. A
// stateless rollout reserves one surge Pod; a persistent app uses Recreate and
// keeps only its existing reservation until the new workload is ready.
func (s *Store) QueueApprovedImageUpdate(ctx context.Context, accountID, subject, appID, key, artifactID string) (server.DeploymentTask, bool, error) {
	return s.queueApprovedImageReplacement(ctx, accountID, subject, appID, key, artifactID, "")
}

// QueueApprovedRevisionRollback restores a previously successful approved
// image revision. A new revision records the restoration as a new operation.
func (s *Store) QueueApprovedRevisionRollback(ctx context.Context, accountID, subject, appID, key, targetRevisionID string) (server.DeploymentTask, bool, error) {
	if _, err := uuid.Parse(targetRevisionID); err != nil {
		return server.DeploymentTask{}, false, &domain.Error{Code: "REVISION_NOT_RESTORABLE", Message: "Select a successful approved image revision from this app."}
	}
	return s.queueApprovedImageReplacement(ctx, accountID, subject, appID, key, "", targetRevisionID)
}

func (s *Store) queueApprovedImageReplacement(ctx context.Context, accountID, subject, appID, key, artifactID, targetRevisionID string) (server.DeploymentTask, bool, error) {
	var task server.DeploymentTask
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		return task, false, &domain.Error{Code: "INVALID_IDEMPOTENCY_KEY", Message: "Send an Idempotency-Key of 8–128 non-space characters."}
	}
	if targetRevisionID == "" {
		if _, err := kubeobjects.NamespaceName(artifactID); err != nil {
			return task, false, &domain.Error{Code: "IMAGE_NOT_APPROVED", Message: "Select an approved image from this account."}
		}
	}
	if targetRevisionID != "" && artifactID != "" {
		return task, false, &domain.Error{Code: "IMAGE_NOT_APPROVED", Message: "Select an approved image from this account."}
	}
	operationKind := "UPDATE_REQUEST"
	if targetRevisionID != "" {
		operationKind = "ROLLBACK_REQUEST"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return task, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var quota domain.Quota
	var enabled, imageEnabled, volumeEnabled bool
	err = tx.QueryRow(ctx, `SELECT enabled,image_deploy_enabled,volume_enabled,quota_cpu_request_milli,quota_cpu_limit_milli,
		quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(
		&enabled, &imageEnabled, &volumeEnabled, &quota.CPURequestMilli, &quota.CPULimitMilli,
		&quota.MemoryMiB, &quota.Pods, &quota.Apps)
	if err != nil {
		return task, false, storeError(err)
	}
	if !enabled {
		return task, false, server.ErrDisabled
	}
	if !imageEnabled {
		return task, false, server.ErrImageDeployDisabled
	}
	var replayKind, replayArtifact, replayOperationKind, replayTargetRevision string
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.account_id::text,t.app_id::text,t.revision_id::text,
		t.desired_state,t.state,t.created_at,t.task_kind,COALESCE(r.image_artifact_id::text,''),
		COALESCE((SELECT o.id::text FROM operations o WHERE o.account_id=t.account_id
		 AND o.app_id=t.app_id AND o.kind IN ('UPDATE_REQUEST','ROLLBACK_REQUEST') AND o.detail->>'taskId'=t.id::text LIMIT 1),''),
		COALESCE((SELECT o.kind FROM operations o WHERE o.account_id=t.account_id
		 AND o.app_id=t.app_id AND o.kind IN ('UPDATE_REQUEST','ROLLBACK_REQUEST') AND o.detail->>'taskId'=t.id::text LIMIT 1),''),
		COALESCE((SELECT o.detail->>'targetRevisionId' FROM operations o WHERE o.account_id=t.account_id
		 AND o.app_id=t.app_id AND o.kind IN ('UPDATE_REQUEST','ROLLBACK_REQUEST') AND o.detail->>'taskId'=t.id::text LIMIT 1),'')
		FROM deployment_tasks t JOIN app_revisions r ON r.id=t.revision_id
		WHERE t.account_id=$1 AND t.idempotency_key=$2`, accountID, key).Scan(
		&task.ID, &task.AccountID, &task.AppID, &task.RevisionID,
		&task.DesiredState, &task.State, &task.CreatedAt, &replayKind, &replayArtifact, &task.OperationID,
		&replayOperationKind, &replayTargetRevision)
	if err == nil {
		if replayKind != "Update" || task.AppID != appID || replayOperationKind != operationKind ||
			(targetRevisionID == "" && replayArtifact != artifactID) ||
			(targetRevisionID != "" && replayTargetRevision != targetRevisionID) {
			return server.DeploymentTask{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used for another operation."}
		}
		return task, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return task, false, storeError(err)
	}
	var spec domain.AppSpec
	var status string
	err = tx.QueryRow(ctx, `SELECT name,image,port,cpu_request_milli,cpu_limit_milli,memory_mib,
		replicas,persistent,COALESCE(volume_id::text,''),COALESCE(mount_path,''),health_path,secret_keys,status
		FROM apps WHERE id=$1 AND account_id=$2 FOR UPDATE`, appID, accountID).Scan(
		&spec.Name, &spec.Image, &spec.Port, &spec.Resources.CPURequestMilli,
		&spec.Resources.CPULimitMilli, &spec.Resources.MemoryMiB, &spec.Resources.Replicas,
		&spec.Persistent, &spec.VolumeID, &spec.MountPath, &spec.HealthPath, &spec.SecretKeys, &status)
	if err != nil {
		return task, false, storeError(err)
	}
	if status != "Running" {
		return task, false, &domain.Error{Code: "APP_NOT_UPDATABLE", Message: "Only running apps can be updated."}
	}
	if spec.Persistent {
		if !volumeEnabled {
			return task, false, server.ErrVolumeDisabled
		}
		var volumeState string
		err = tx.QueryRow(ctx, `SELECT state FROM student_volumes WHERE id=$1 AND account_id=$2 FOR SHARE`,
			spec.VolumeID, accountID).Scan(&volumeState)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && volumeState != "Ready") {
			return task, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Wait until the bound volume is ready."}
		}
		if err != nil {
			return task, false, storeError(err)
		}
	}
	var priorRevisionID, priorDigest string
	err = tx.QueryRow(ctx, `SELECT rr.revision_id::text,r.platform_image_digest
		FROM resource_reservations rr JOIN app_revisions r ON r.id=rr.revision_id
		WHERE rr.account_id=$1 AND rr.app_id=$2 FOR UPDATE OF rr`, accountID, appID).Scan(&priorRevisionID, &priorDigest)
	if err != nil {
		return task, false, storeError(err)
	}
	if targetRevisionID != "" {
		if priorRevisionID == targetRevisionID {
			return task, false, &domain.Error{Code: "REVISION_ALREADY_RUNNING", Message: "This revision is already running."}
		}
		err = tx.QueryRow(ctx, `SELECT r.image_artifact_id::text FROM app_revisions r
			WHERE r.id=$1 AND r.account_id=$2 AND r.app_id=$3 AND r.source_kind='image'
			AND EXISTS(SELECT 1 FROM deployment_tasks t WHERE t.revision_id=r.id
			 AND t.account_id=$2 AND t.app_id=$3 AND t.state='Succeeded' AND t.task_kind IN ('Deploy','Update'))`,
			targetRevisionID, accountID, appID).Scan(&artifactID)
		if errors.Is(err, pgx.ErrNoRows) {
			return task, false, &domain.Error{Code: "REVISION_NOT_RESTORABLE", Message: "Select a successful approved image revision from this app."}
		}
		if err != nil {
			return task, false, storeError(err)
		}
	}
	var scanStatus, platformDigest, manifestDigest, reportHash, policyVersion string
	err = tx.QueryRow(ctx, `SELECT scan_status,COALESCE(harbor_reference,''),platform_digest,
		COALESCE(scan_report_sha256,''),COALESCE(scan_summary->>'policyVersion','')
		FROM image_artifacts WHERE id=$1 AND account_id=$2 AND gc_state='Active' FOR SHARE`, artifactID, accountID).Scan(
		&scanStatus, &platformDigest, &manifestDigest, &reportHash, &policyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, &domain.Error{Code: "IMAGE_NOT_APPROVED", Message: "Select an approved image from this account."}
	}
	if err != nil {
		return task, false, storeError(err)
	}
	project, projectErr := harbor.AccountProjectName(accountID)
	if scanStatus != "Approved" || !imageDigestPattern.MatchString(reportHash) ||
		policyVersion != harbor.PolicyVersion || projectErr != nil ||
		!strings.HasPrefix(platformDigest, harbor.RegistryHost+"/"+project+"/") ||
		!strings.HasSuffix(platformDigest, "@"+manifestDigest) ||
		kubeobjects.ValidatePlatformDigest(platformDigest) != nil {
		return task, false, &domain.Error{Code: "IMAGE_NOT_APPROVED", Message: "Select an approved image from this account."}
	}
	if platformDigest == priorDigest {
		return task, false, &domain.Error{Code: "IMAGE_ALREADY_RUNNING", Message: "This image is already running."}
	}
	if spec.Persistent {
		_, err = kubeobjects.TrustedWorkload(accountID, appID, spec, platformDigest)
	} else {
		_, err = kubeobjects.RollingWorkload(accountID, appID, spec, platformDigest)
	}
	if err != nil {
		return task, false, err
	}
	var accountUsed, globalUsed domain.Usage
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(cpu_request_milli),0),COALESCE(SUM(cpu_limit_milli),0),
		COALESCE(SUM(memory_mib),0),COALESCE(SUM(pods),0),COUNT(*)
		FROM (SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM resource_reservations WHERE account_id=$1
		 UNION ALL SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM rollout_reservations WHERE account_id=$1) u`,
		accountID).Scan(&accountUsed.CPURequestMilli, &accountUsed.CPULimitMilli,
		&accountUsed.MemoryMiB, &accountUsed.Pods, &accountUsed.Apps)
	if err != nil {
		return task, false, storeError(err)
	}
	// Temporary surge reservations consume Pod resources but no application slot.
	err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations WHERE account_id=$1`, accountID).Scan(&accountUsed.Apps)
	if err != nil {
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
	var surge domain.Usage
	if !spec.Persistent {
		surge = domain.Usage{CPURequestMilli: spec.Resources.CPURequestMilli,
			CPULimitMilli: spec.Resources.CPULimitMilli, MemoryMiB: spec.Resources.MemoryMiB, Pods: 1}
	}
	if err = checkNoPendingQuotaChange(ctx, tx, accountID); err != nil {
		return task, false, err
	}
	if err = domain.CheckAdmission(accountUsed, globalUsed, surge, quota, globalQuota); err != nil {
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
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return task, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec,source_kind,image_artifact_id)
		VALUES($1,$2,$3,$4,$5,$6::jsonb,'image',$7)`, revisionID, accountID, appID, revision, platformDigest, specJSON, artifactID)
	if err != nil {
		return task, false, storeError(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,idempotency_key,desired_state,task_kind,prior_revision_id)
		VALUES($1,$2,$3,$4,$5,'Running','Update',$6) RETURNING created_at`,
		taskID, accountID, appID, revisionID, key, priorRevisionID).Scan(&task.CreatedAt)
	if err != nil {
		return task, false, storeError(err)
	}
	if !spec.Persistent {
		_, err = tx.Exec(ctx, `INSERT INTO rollout_reservations(update_task_id,account_id,app_id,cpu_request_milli,cpu_limit_milli,memory_mib)
			VALUES($1,$2,$3,$4,$5,$6)`, taskID, accountID, appID,
			surge.CPURequestMilli, surge.CPULimitMilli, surge.MemoryMiB)
		if err != nil {
			return task, false, storeError(err)
		}
		_, err = tx.Exec(ctx, `UPDATE global_budget SET reserved_cpu_request_milli=reserved_cpu_request_milli+$1,
			reserved_cpu_limit_milli=reserved_cpu_limit_milli+$2,reserved_memory_mib=reserved_memory_mib+$3,
			reserved_pods=reserved_pods+1 WHERE id=true`, surge.CPURequestMilli, surge.CPULimitMilli, surge.MemoryMiB)
		if err != nil {
			return task, false, storeError(err)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE apps SET status='Queued',updated_at=now() WHERE id=$1 AND account_id=$2`, appID, accountID)
	if err != nil {
		return task, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(id,task_id,event_type) VALUES($1,$2,'RECONCILE_DEPLOYMENT')`, outboxID, taskID)
	if err != nil {
		return task, false, storeError(err)
	}
	opDetail, err := json.Marshal(map[string]string{"appId": appID, "taskId": taskID, "revisionId": revisionID,
		"priorRevisionId": priorRevisionID, "artifactId": artifactID, "targetRevisionId": targetRevisionID})
	if err != nil {
		return task, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,$5,'Queued',$6::jsonb)`, opID, accountID, appID, subject, operationKind, opDetail)
	if err != nil {
		return task, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return task, false, storeError(err)
	}
	return server.DeploymentTask{ID: taskID, OperationID: opID, AccountID: accountID, AppID: appID,
		RevisionID: revisionID, DesiredState: "Running", State: "Queued", CreatedAt: task.CreatedAt}, false, nil
}
