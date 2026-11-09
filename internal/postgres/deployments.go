package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/server"
)

type DeploymentTask = server.DeploymentTask

// QueueTrustedDeploy writes the revision, budget reservation, task, audit
// operation and outbox event in one transaction. It never calls Kubernetes.
// The caller supplies a digest from the platform's approved image catalog.
func (s *Store) QueueTrustedDeploy(ctx context.Context, accountID, subject, appID, key, platformDigest string, platformPort int) (DeploymentTask, bool, error) {
	return s.queueDeploy(ctx, accountID, subject, appID, key,
		"trusted", "", platformDigest, platformPort)
}

// QueueApprovedImageDeploy accepts only an account-owned Approved artifact.
// The artifact lookup, revision, budget reservation and task share one SQL
// transaction, so a client cannot substitute its own Harbor reference.
func (s *Store) QueueApprovedImageDeploy(ctx context.Context, accountID, subject,
	appID, key, artifactID string) (DeploymentTask, bool, error) {
	if _, err := kubeobjects.NamespaceName(artifactID); err != nil {
		return DeploymentTask{}, false, &domain.Error{Code: "IMAGE_NOT_APPROVED",
			Message: "Select an approved image from this account."}
	}
	return s.queueDeploy(ctx, accountID, subject, appID, key,
		"image", artifactID, "", 0)
}

func (s *Store) queueDeploy(ctx context.Context, accountID, subject, appID, key,
	sourceKind, artifactID, platformDigest string, platformPort int) (DeploymentTask, bool, error) {
	var task DeploymentTask
	if sourceKind == "trusted" && (platformPort < 1 || platformPort > 65535) {
		return task, false, &domain.Error{Code: "INVALID_PLATFORM_PORT", Message: "The platform sample port is invalid."}
	}
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		return task, false, &domain.Error{Code: "INVALID_IDEMPOTENCY_KEY", Message: "Send an Idempotency-Key of 8–128 non-space characters."}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return task, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var quota domain.Quota
	var enabled, imageDeployEnabled, volumeEnabled bool
	err = tx.QueryRow(ctx, `SELECT enabled,image_deploy_enabled,volume_enabled,quota_cpu_request_milli,quota_cpu_limit_milli,
        quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(
		&enabled, &imageDeployEnabled, &volumeEnabled, &quota.CPURequestMilli, &quota.CPULimitMilli,
		&quota.MemoryMiB, &quota.Pods, &quota.Apps)
	if err != nil {
		return task, false, storeError(err)
	}
	if !enabled {
		return task, false, server.ErrDisabled
	}
	if sourceKind == "image" && !imageDeployEnabled {
		return task, false, server.ErrImageDeployDisabled
	}
	var replayDigest, replaySource, replayArtifact string
	var replayPort int
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.account_id::text,t.app_id::text,t.revision_id::text,
	        t.desired_state,t.state,t.created_at,r.platform_image_digest,(r.spec->>'port')::integer,
	        r.source_kind,COALESCE(r.image_artifact_id::text,''),
	        (SELECT o.id::text FROM operations o WHERE o.account_id=t.account_id
	            AND o.app_id=t.app_id AND o.kind='DEPLOY_REQUEST'
	            AND o.detail->>'taskId'=t.id::text LIMIT 1)
        FROM deployment_tasks t JOIN app_revisions r ON r.id=t.revision_id
        WHERE t.account_id=$1 AND t.idempotency_key=$2`, accountID, key).Scan(
		&task.ID, &task.AccountID, &task.AppID, &task.RevisionID,
		&task.DesiredState, &task.State, &task.CreatedAt, &replayDigest, &replayPort,
		&replaySource, &replayArtifact, &task.OperationID)
	if err == nil {
		match := replaySource == sourceKind && task.AppID == appID && task.DesiredState == "Running"
		if sourceKind == "trusted" {
			match = match && replayDigest == platformDigest && replayPort == platformPort
		} else {
			match = match && replayArtifact == artifactID
		}
		if !match {
			return DeploymentTask{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used for another deployment."}
		}
		return task, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeploymentTask{}, false, storeError(err)
	}
	var spec domain.AppSpec
	var status string
	err = tx.QueryRow(ctx, `SELECT name,image,port,cpu_request_milli,cpu_limit_milli,memory_mib,
	        replicas,persistent,COALESCE(volume_id::text,''),COALESCE(mount_path,''),health_path,secret_keys,status
		FROM apps WHERE id=$1 AND account_id=$2 FOR UPDATE`, appID, accountID).Scan(
		&spec.Name, &spec.Image, &spec.Port, &spec.Resources.CPURequestMilli,
		&spec.Resources.CPULimitMilli, &spec.Resources.MemoryMiB,
		&spec.Resources.Replicas, &spec.Persistent, &spec.VolumeID, &spec.MountPath, &spec.HealthPath, &spec.SecretKeys, &status)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	if status != "Draft" && status != "Stopped" && status != "Failed" {
		return DeploymentTask{}, false, &domain.Error{Code: "APP_NOT_DEPLOYABLE", Message: "Only draft, stopped or cleaned-up failed apps can start a new deployment."}
	}
	if spec.Persistent {
		if !volumeEnabled {
			return DeploymentTask{}, false, server.ErrVolumeDisabled
		}
		if err := domain.ValidateVolumeBinding(true, spec.VolumeID, spec.MountPath); err != nil {
			return DeploymentTask{}, false, err
		}
		var volumeState string
		err = tx.QueryRow(ctx, `SELECT state FROM student_volumes
			WHERE account_id=$1 AND id=$2 FOR SHARE`, accountID, spec.VolumeID).Scan(&volumeState)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && volumeState != "Ready") {
			return DeploymentTask{}, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Wait until the bound volume is ready."}
		}
		if err != nil {
			return DeploymentTask{}, false, storeError(err)
		}
	}
	if sourceKind == "image" {
		var reportHash, policyVersion, scanStatus, platformManifestDigest string
		err = tx.QueryRow(ctx, `SELECT scan_status,COALESCE(harbor_reference,''),
	            platform_digest,COALESCE(scan_report_sha256,''),
	            COALESCE(scan_summary->>'policyVersion','')
            FROM image_artifacts WHERE id=$1 AND account_id=$2 AND gc_state='Active' FOR SHARE`,
			artifactID, accountID).Scan(&scanStatus, &platformDigest,
			&platformManifestDigest, &reportHash, &policyVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return DeploymentTask{}, false, &domain.Error{Code: "IMAGE_NOT_APPROVED",
				Message: "Select an approved image from this account."}
		}
		if err != nil {
			return DeploymentTask{}, false, storeError(err)
		}
		project, projectErr := harbor.AccountProjectName(accountID)
		if scanStatus != "Approved" || !imageDigestPattern.MatchString(reportHash) ||
			policyVersion != harbor.PolicyVersion || projectErr != nil ||
			!strings.HasPrefix(platformDigest, harbor.RegistryHost+"/"+project+"/") ||
			!strings.HasSuffix(platformDigest, "@"+platformManifestDigest) ||
			kubeobjects.ValidatePlatformDigest(platformDigest) != nil {
			return DeploymentTask{}, false, &domain.Error{Code: "IMAGE_NOT_APPROVED",
				Message: "Select an approved image from this account."}
		}
		platformPort = spec.Port
	} else {
		spec.Port = platformPort
	}
	if _, err = kubeobjects.TrustedWorkload(accountID, appID, spec, platformDigest); err != nil {
		return DeploymentTask{}, false, err
	}
	usage := spec.Resources.Usage()
	var accountUsed domain.Usage
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(cpu_request_milli),0),COALESCE(SUM(cpu_limit_milli),0),
		COALESCE(SUM(memory_mib),0),COALESCE(SUM(pods),0)
		FROM (SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM resource_reservations WHERE account_id=$1
		 UNION ALL SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods FROM rollout_reservations WHERE account_id=$1) u`, accountID).Scan(
		&accountUsed.CPURequestMilli, &accountUsed.CPULimitMilli,
		&accountUsed.MemoryMiB, &accountUsed.Pods)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations WHERE account_id=$1`, accountID).Scan(&accountUsed.Apps); err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	var globalQuota domain.Quota
	var globalUsed domain.Usage
	err = tx.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods,
        reserved_cpu_request_milli,reserved_cpu_limit_milli,reserved_memory_mib,reserved_pods
        FROM global_budget WHERE id=true FOR UPDATE`).Scan(
		&globalQuota.CPURequestMilli, &globalQuota.CPULimitMilli,
		&globalQuota.MemoryMiB, &globalQuota.Pods,
		&globalUsed.CPURequestMilli, &globalUsed.CPULimitMilli,
		&globalUsed.MemoryMiB, &globalUsed.Pods)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	if err = checkNoPendingQuotaChange(ctx, tx, accountID); err != nil {
		return DeploymentTask{}, false, err
	}
	if err = domain.CheckAdmission(accountUsed, globalUsed, usage, quota, globalQuota); err != nil {
		return DeploymentTask{}, false, err
	}
	var existing bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM resource_reservations WHERE app_id=$1)`, appID).Scan(&existing)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	if existing {
		return DeploymentTask{}, false, &domain.Error{Code: "APP_RESERVATION_EXISTS", Message: "A prior deployment still holds resources for this app."}
	}
	var revision int
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM app_revisions WHERE app_id=$1`, appID).Scan(&revision)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	revisionID, err := server.NewID()
	if err != nil {
		return DeploymentTask{}, false, err
	}
	taskID, err := server.NewID()
	if err != nil {
		return DeploymentTask{}, false, err
	}
	outboxID, err := server.NewID()
	if err != nil {
		return DeploymentTask{}, false, err
	}
	opID, err := server.NewID()
	if err != nil {
		return DeploymentTask{}, false, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return DeploymentTask{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec,
        source_kind,image_artifact_id)
	        VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,NULLIF($8,'')::uuid)`,
		revisionID, accountID, appID, revision, platformDigest, specJSON, sourceKind, artifactID)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO resource_reservations(app_id,account_id,revision_id,cpu_request_milli,
        cpu_limit_milli,memory_mib,pods) VALUES($1,$2,$3,$4,$5,$6,$7)`, appID, accountID, revisionID,
		usage.CPURequestMilli, usage.CPULimitMilli, usage.MemoryMiB, usage.Pods)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE global_budget SET
        reserved_cpu_request_milli=reserved_cpu_request_milli+$1,
        reserved_cpu_limit_milli=reserved_cpu_limit_milli+$2,
        reserved_memory_mib=reserved_memory_mib+$3,
        reserved_pods=reserved_pods+$4 WHERE id=true`,
		usage.CPURequestMilli, usage.CPULimitMilli, usage.MemoryMiB, usage.Pods)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE apps SET status='Queued',updated_at=now() WHERE id=$1 AND account_id=$2`, appID, accountID)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,idempotency_key,desired_state)
        VALUES($1,$2,$3,$4,$5,'Running') RETURNING created_at`,
		taskID, accountID, appID, revisionID, key).Scan(&task.CreatedAt)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(id,task_id,event_type)
        VALUES($1,$2,'RECONCILE_DEPLOYMENT')`, outboxID, taskID)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	opDetail := map[string]string{"appId": appID, "taskId": taskID, "revisionId": revisionID,
		"sourceKind": sourceKind}
	if sourceKind == "image" {
		opDetail["artifactId"] = artifactID
	}
	opJSON, err := json.Marshal(opDetail)
	if err != nil {
		return DeploymentTask{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,$4,'DEPLOY_REQUEST','Queued',$5::jsonb)`, opID, accountID, appID, subject,
		opJSON)
	if err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return DeploymentTask{}, false, storeError(err)
	}
	return DeploymentTask{ID: taskID, OperationID: opID, AccountID: accountID, AppID: appID, RevisionID: revisionID,
		DesiredState: "Running", State: "Queued", CreatedAt: task.CreatedAt}, false, nil
}
