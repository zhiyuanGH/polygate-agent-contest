package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

// CompleteLeasedSourceBuild atomically registers the verified image, creates
// its scan task, and marks the source build successful. A stale worker cannot
// leave an orphan image artifact or consume the account's image quota.
func (s *Store) CompleteLeasedSourceBuild(ctx context.Context, task SourceBuildTask,
	workerID string, image registry.Image) (server.ImageArtifact, error) {
	var empty server.ImageArtifact
	if err := validateResolvedImage(image); err != nil {
		return empty, err
	}
	if task.ID == "" || task.AccountID == "" || workerID == "" ||
		task.Attempt < 1 || task.Attempt > maxSourceBuildAttempts ||
		strings.TrimSpace(workerID) != workerID || len(workerID) > 128 {
		return empty, errors.New("invalid leased build identity")
	}
	project, err := harbor.AccountProjectName(task.AccountID)
	if err != nil || image.RegistryHost != harbor.RegistryHost ||
		image.PinnedReference != harbor.RegistryHost+"/"+project+"/agent@"+image.ManifestDigest ||
		image.SubmittedReference != image.PinnedReference ||
		image.PlatformDigest != image.ManifestDigest {
		return empty, errors.New("build image is outside its account project")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return empty, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, buildEnabled bool
	if err = tx.QueryRow(ctx, `SELECT enabled,source_build_enabled FROM accounts WHERE id=$1 FOR UPDATE`,
		task.AccountID).Scan(&enabled, &buildEnabled); err != nil {
		return empty, storeError(err)
	}
	if !enabled {
		return empty, server.ErrDisabled
	}
	if !buildEnabled {
		return empty, &domain.Error{Code: "BUILD_NOT_ENABLED", Message: "Source builds are not enabled for this account."}
	}
	var deletingDigest bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_artifacts
        WHERE account_id=$1 AND platform_digest=$2 AND gc_state<>'Active')`,
		task.AccountID, image.PlatformDigest).Scan(&deletingDigest); err != nil {
		return empty, storeError(err)
	}
	if deletingDigest {
		return empty, &domain.Error{Code: "IMAGE_CLEANUP_IN_PROGRESS",
			Message: "This digest is being removed. Retry the build shortly."}
	}
	var leasedAccount string
	err = tx.QueryRow(ctx, `SELECT account_id::text FROM source_build_tasks
		WHERE id=$1 AND account_id=$2 AND locked_by=$3 AND attempt=$4
		AND state='Leased' AND lease_until>clock_timestamp() FOR UPDATE`,
		task.ID, task.AccountID, workerID, task.Attempt).Scan(&leasedAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, server.ErrNotFound
	}
	if err != nil {
		return empty, storeError(err)
	}
	artifact, err := scanImage(tx.QueryRow(ctx, `SELECT `+imageColumns+`
		FROM image_artifacts WHERE account_id=$1 AND pinned_reference=$2 AND gc_state='Active' FOR SHARE`,
		leasedAccount, image.PinnedReference))
	if errors.Is(err, pgx.ErrNoRows) {
		var deleting bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_artifacts
			WHERE account_id=$1 AND pinned_reference=$2 AND gc_state<>'Active')`,
			leasedAccount, image.PinnedReference).Scan(&deleting); err != nil {
			return empty, storeError(err)
		}
		if deleting {
			return empty, &domain.Error{Code: "IMAGE_CLEANUP_IN_PROGRESS",
				Message: "This digest is being removed. Retry the build shortly."}
		}
		var count int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM image_artifacts WHERE account_id=$1 AND gc_state='Active'`,
			leasedAccount).Scan(&count); err != nil {
			return empty, storeError(err)
		}
		if count >= maxImageArtifactsPerAccount {
			return empty, &domain.Error{Code: "IMAGE_ARTIFACT_LIMIT", Field: "reference",
				Message: "This account has reached its image inspection limit.", Current: count,
				Allowed: maxImageArtifactsPerAccount, Requested: 1}
		}
		id, idErr := server.NewID()
		if idErr != nil {
			return empty, idErr
		}
		artifact, err = scanImage(tx.QueryRow(ctx, `INSERT INTO image_artifacts(id,account_id,
			submitted_reference,pinned_reference,platform_digest,registry_host,configured_user)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+imageColumns,
			id, leasedAccount, image.SubmittedReference, image.PinnedReference,
			image.PlatformDigest, image.RegistryHost, image.ConfiguredUser))
		if err != nil {
			return empty, storeError(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO image_scan_tasks(artifact_id,account_id)
			VALUES($1,$2)`, artifact.ID, leasedAccount); err != nil {
			return empty, storeError(err)
		}
		opID, idErr := server.NewID()
		if idErr != nil {
			return empty, idErr
		}
		detail, jsonErr := json.Marshal(map[string]string{"artifactId": artifact.ID,
			"submittedReference": artifact.SubmittedReference,
			"pinnedReference":    artifact.PinnedReference})
		if jsonErr != nil {
			return empty, jsonErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
			VALUES($1,$2,'source-build','IMAGE_INSPECT','PendingScan',$3::jsonb)`,
			opID, leasedAccount, detail); err != nil {
			return empty, storeError(err)
		}
	} else if err != nil {
		return empty, storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE source_build_tasks SET state='Succeeded',locked_by=NULL,
		lease_until=NULL,error_code=NULL,result_image_artifact_id=$5,updated_at=now()
		WHERE id=$1 AND account_id=$2 AND locked_by=$3 AND attempt=$4
		AND state='Leased' AND lease_until>clock_timestamp()`,
		task.ID, leasedAccount, workerID, task.Attempt, artifact.ID)
	if err != nil {
		return empty, storeError(err)
	}
	if result.RowsAffected() != 1 {
		return empty, server.ErrNotFound
	}
	result, err = tx.Exec(ctx, `UPDATE operations SET state='Succeeded',
		detail=(detail - 'errorCode') || jsonb_build_object('imageArtifactId',$3::text)
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND detail->>'taskId'=$2`,
		leasedAccount, task.ID, artifact.ID)
	if err != nil {
		return empty, storeError(err)
	}
	if result.RowsAffected() != 1 {
		return empty, errors.New("source build operation missing")
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	return artifact, nil
}
