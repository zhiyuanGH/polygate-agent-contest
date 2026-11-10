package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// QueueImageScanRetry starts another bounded scan cycle for an account-owned
// failed artifact. A completed retry request is audited separately from its scan
// outcome, so the original failure and the subsequent scanner result remain in
// the activity history. The account lock serializes retries and image deletion.
func (s *Store) QueueImageScanRetry(ctx context.Context, accountID, subject, artifactID, key string) (server.ImageArtifact, bool, error) {
	var empty server.ImageArtifact
	id, err := uuid.Parse(artifactID)
	if err != nil {
		return empty, false, server.ErrNotFound
	}
	artifactID = id.String()
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		return empty, false, &domain.Error{Code: "INVALID_IDEMPOTENCY_KEY", Message: "Send an Idempotency-Key of 8–128 non-space characters."}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return empty, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	// NO KEY UPDATE still serializes account mutations, but permits the KEY
	// SHARE check taken by the scanner's operations.account_id foreign key.
	// FOR UPDATE here could deadlock against a scanner holding the task lock.
	if err = tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accountID).Scan(&enabled); err != nil {
		return empty, false, storeError(err)
	}
	if !enabled {
		return empty, false, server.ErrDisabled
	}

	// The scanner locks its task before the artifact when saving evidence. Use
	// that same order here, rather than introducing an artifact/task deadlock.
	var taskState, failureCode string
	var attempts int
	var unleased bool
	err = tx.QueryRow(ctx, `SELECT state,attempt,COALESCE(error_code,''),
		locked_by IS NULL AND lease_until IS NULL FROM image_scan_tasks
		WHERE artifact_id=$1 AND account_id=$2 FOR UPDATE`, artifactID, accountID).
		Scan(&taskState, &attempts, &failureCode, &unleased)
	hasTask := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, storeError(err)
	}
	var scanStatus, gcState string
	if err = tx.QueryRow(ctx, `SELECT scan_status,gc_state FROM image_artifacts
		WHERE id=$1 AND account_id=$2 FOR UPDATE`, artifactID, accountID).Scan(&scanStatus, &gcState); err != nil {
		return empty, false, storeError(err)
	}
	if gcState != "Active" {
		return empty, false, server.ErrImageCleanupInProgress
	}
	var previousArtifactID string
	err = tx.QueryRow(ctx, `SELECT detail->>'artifactId' FROM operations
		WHERE account_id=$1 AND kind='IMAGE_SCAN_RETRY'
		AND detail->>'idempotencyKey'=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, accountID, key).
		Scan(&previousArtifactID)
	if err == nil {
		if previousArtifactID != artifactID {
			return empty, false, server.ErrImageScanRetryKeyReused
		}
		item, err := imageScanRetryArtifact(ctx, tx, accountID, artifactID)
		if err != nil {
			return empty, false, err
		}
		return item, true, storeError(tx.Commit(ctx))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, storeError(err)
	}
	if !hasTask || scanStatus != "Failed" || taskState != "Failed" || !unleased {
		return empty, false, server.ErrImageScanNotRetryable
	}
	if _, err = tx.Exec(ctx, `UPDATE image_artifacts SET scan_status='PendingScan',
		harbor_reference=NULL,scan_report_sha256=NULL,scan_report_id=NULL,
		scanner_name=NULL,scan_completed_at=NULL,scan_summary=NULL,
		scan_report_body=NULL,scan_report_body_sha256=NULL
		WHERE id=$1 AND account_id=$2`, artifactID, accountID); err != nil {
		return empty, false, storeError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE image_scan_tasks SET state='Queued',attempt=0,
		locked_by=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),
		error_code=NULL,updated_at=clock_timestamp()
		WHERE artifact_id=$1 AND account_id=$2`, artifactID, accountID); err != nil {
		return empty, false, storeError(err)
	}
	opID, err := server.NewID()
	if err != nil {
		return empty, false, err
	}
	// Succeeded describes the requeue action, not an approval. IMAGE_SCAN
	// records the worker's eventual Approved/Rejected/Failed outcome separately.
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,'IMAGE_SCAN_RETRY','Succeeded',jsonb_build_object(
		'artifactId',$4::text,'idempotencyKey',$5::text,'previousErrorCode',$6::text,
		'previousAttempts',$7::integer,'scanState','Queued'))`,
		opID, accountID, subject, artifactID, key, failureCode, attempts); err != nil {
		return empty, false, storeError(err)
	}
	item, err := imageScanRetryArtifact(ctx, tx, accountID, artifactID)
	if err != nil {
		return empty, false, err
	}
	return item, false, storeError(tx.Commit(ctx))
}

func imageScanRetryArtifact(ctx context.Context, tx pgx.Tx, accountID, artifactID string) (server.ImageArtifact, error) {
	item, err := scanImageWithTask(tx.QueryRow(ctx, `SELECT `+imageListColumns+`,
		COALESCE(t.state,''),COALESCE(t.error_code,''),image_artifacts.scan_summary
		FROM image_artifacts LEFT JOIN image_scan_tasks t ON t.artifact_id=image_artifacts.id
		AND t.account_id=image_artifacts.account_id
		WHERE image_artifacts.id=$1 AND image_artifacts.account_id=$2`, artifactID, accountID))
	return item, storeError(err)
}
