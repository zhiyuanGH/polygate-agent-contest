package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// ImageGCCandidate is a single account-owned Harbor artifact. The row has
// already been switched to Deleting before it leaves the database transaction.
type ImageGCCandidate struct {
	ID        string
	AccountID string
	Digest    string
}

// RequestImageDeletion makes an unreferenced, terminal image unavailable
// immediately. The GC worker later removes its Harbor artifact.
func (s *Store) RequestImageDeletion(ctx context.Context, accountID, subject, artifactID string) error {
	if _, err := uuid.Parse(artifactID); err != nil {
		return server.ErrNotFound
	}
	operationID, err := server.NewID()
	if err != nil {
		return err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&locked); err != nil {
		return storeError(err)
	}
	var state, status, digest string
	err = tx.QueryRow(ctx, `SELECT gc_state,scan_status,platform_digest FROM image_artifacts
        WHERE id=$1 AND account_id=$2 FOR UPDATE`, artifactID, accountID).Scan(&state, &status, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrNotFound
	}
	if err != nil {
		return storeError(err)
	}
	if state == "Requested" || state == "Deleting" {
		return storeError(tx.Commit(ctx))
	}
	if status == "PendingScan" {
		return &domain.Error{Code: "IMAGE_SCAN_IN_PROGRESS", Message: "Wait for the security check before deleting this image."}
	}
	var referenced bool
	err = tx.QueryRow(ctx, `SELECT
        EXISTS(SELECT 1 FROM app_revisions WHERE image_artifact_id=$1)
        OR EXISTS(SELECT 1 FROM source_build_tasks
            WHERE result_image_artifact_id=$1 AND state<>'Succeeded')
        OR EXISTS(SELECT 1 FROM image_artifacts WHERE account_id=$2 AND platform_digest=$3 AND id<>$1)
        OR EXISTS(SELECT 1 FROM source_build_tasks WHERE account_id=$2 AND state IN ('Preparing','Queued','Leased'))
        OR NOT EXISTS(SELECT 1 FROM image_scan_tasks WHERE artifact_id=$1 AND state IN ('Succeeded','Failed'))`,
		artifactID, accountID, digest).Scan(&referenced)
	if err != nil {
		return storeError(err)
	}
	if referenced {
		return &domain.Error{Code: "IMAGE_STILL_IN_USE",
			Message: "This image is referenced by an app or its account has an active build."}
	}
	if _, err = tx.Exec(ctx, `UPDATE source_build_tasks
        SET result_image_artifact_id=NULL,result_image_deleted_at=clock_timestamp()
        WHERE account_id=$1 AND result_image_artifact_id=$2 AND state='Succeeded'`,
		accountID, artifactID); err != nil {
		return storeError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE image_artifacts SET gc_state='Requested',gc_reason='Student'
        WHERE id=$1 AND account_id=$2`, artifactID, accountID); err != nil {
		return storeError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,'IMAGE_DELETE_REQUEST','Queued',
            jsonb_build_object('artifactId',$4::text,'digest',$5::text))`,
		operationID, accountID, subject, artifactID, digest); err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}

// ClaimExpiredImage protects every image referenced by an app revision or a
// retained source build. Two newest approved images per account remain in the
// library even when no app uses them. A claimed row cannot be deployed.
func (s *Store) ClaimExpiredImage(ctx context.Context, accountScope string) (ImageGCCandidate, bool, error) {
	return s.claimImageForGC(ctx, accountScope, true)
}

func (s *Store) ClaimRequestedImage(ctx context.Context, accountScope string) (ImageGCCandidate, bool, error) {
	return s.claimImageForGC(ctx, accountScope, false)
}

func (s *Store) claimImageForGC(ctx context.Context, accountScope string, includeRetention bool) (ImageGCCandidate, bool, error) {
	var item ImageGCCandidate
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT account_id::text FROM image_artifacts
        WHERE ($1='' OR account_id::text=$1) AND
          (gc_state='Requested'
           OR (gc_state='Deleting' AND gc_started_at<clock_timestamp()-interval '1 hour')
           OR ($2 AND gc_state='Active' AND
              ((scan_status='Rejected' AND created_at<clock_timestamp()-interval '14 days')
               OR (scan_status='Approved' AND created_at<clock_timestamp()-interval '30 days'))))
        ORDER BY account_id::text LIMIT 500`, accountScope, includeRetention)
	if err != nil {
		return item, false, storeError(err)
	}
	accounts := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return item, false, storeError(err)
		}
		accounts = append(accounts, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return item, false, storeError(err)
	}
	for _, accountID := range accounts {
		tx, beginErr := s.Pool.BeginTx(ctx, pgx.TxOptions{})
		if beginErr != nil {
			return item, false, storeError(beginErr)
		}
		var locked string
		err = tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE id=$1
            FOR UPDATE SKIP LOCKED`, accountID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return item, false, storeError(err)
		}
		// Lock the account before its image, matching submission and build
		// completion. Recheck every reference after obtaining that lock.
		err = tx.QueryRow(ctx, `WITH approved AS (
            SELECT id,row_number() OVER (PARTITION BY account_id ORDER BY created_at DESC,id DESC) AS newest
            FROM image_artifacts WHERE account_id=$1 AND scan_status='Approved' AND gc_state='Active'
        )
        SELECT i.id::text,i.account_id::text,i.platform_digest
        FROM image_artifacts i LEFT JOIN approved a ON a.id=i.id
        WHERE i.account_id=$1
          AND (
            i.gc_state='Requested'
            OR
            (i.gc_state='Deleting' AND i.gc_started_at<clock_timestamp()-interval '1 hour')
            OR ($2 AND i.gc_state='Active' AND (
                (i.scan_status='Rejected' AND i.created_at<clock_timestamp()-interval '14 days')
                OR (i.scan_status='Approved' AND i.created_at<clock_timestamp()-interval '30 days'
                    AND a.newest>2)))
          )
          AND EXISTS (SELECT 1 FROM image_scan_tasks t WHERE t.artifact_id=i.id
              AND (t.state='Succeeded' OR (i.gc_state<>'Active' AND t.state='Failed')))
          AND NOT EXISTS (SELECT 1 FROM source_build_tasks active
              WHERE active.account_id=i.account_id AND active.state IN ('Preparing','Queued','Leased'))
          AND NOT EXISTS (SELECT 1 FROM app_revisions r WHERE r.image_artifact_id=i.id)
          AND NOT EXISTS (SELECT 1 FROM source_build_tasks b WHERE b.result_image_artifact_id=i.id)
          AND NOT EXISTS (SELECT 1 FROM image_artifacts other
              WHERE other.account_id=i.account_id AND other.platform_digest=i.platform_digest AND other.id<>i.id)
			ORDER BY CASE WHEN i.gc_state='Requested' THEN 0 ELSE 1 END,i.created_at,i.id
            LIMIT 1 FOR UPDATE OF i SKIP LOCKED`, accountID, includeRetention).
			Scan(&item.ID, &item.AccountID, &item.Digest)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return item, false, storeError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE image_artifacts SET gc_state='Deleting',gc_started_at=clock_timestamp()
            WHERE id=$1 AND account_id=$2`, item.ID, item.AccountID); err != nil {
			_ = tx.Rollback(ctx)
			return item, false, storeError(err)
		}
		if err = tx.Commit(ctx); err != nil {
			return item, false, storeError(err)
		}
		return item, true, nil
	}
	return ImageGCCandidate{}, false, nil
}

// FinishExpiredImage records the deletion and removes database metadata only
// after Harbor confirms that the digest has gone. Its FK guards are repeated
// here so a future schema change cannot silently make deletion unsafe.
func (s *Store) FinishExpiredImage(ctx context.Context, item ImageGCCandidate) error {
	operationID, err := server.NewID()
	if err != nil {
		return err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE id=$1 FOR UPDATE`,
		item.AccountID).Scan(&locked); err != nil {
		return storeError(err)
	}
	var state, digest, reason string
	err = tx.QueryRow(ctx, `SELECT gc_state,platform_digest,gc_reason FROM image_artifacts
        WHERE id=$1 AND account_id=$2 FOR UPDATE`, item.ID, item.AccountID).Scan(&state, &digest, &reason)
	if err != nil {
		return storeError(err)
	}
	if state != "Deleting" || digest != item.Digest {
		return errors.New("image retention candidate changed")
	}
	var referenced bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app_revisions WHERE image_artifact_id=$1)
        OR EXISTS(SELECT 1 FROM source_build_tasks WHERE result_image_artifact_id=$1)
        OR EXISTS(SELECT 1 FROM image_artifacts
            WHERE account_id=$2 AND platform_digest=$3 AND id<>$1)`, item.ID, item.AccountID, item.Digest).
		Scan(&referenced)
	if err != nil {
		return storeError(err)
	}
	if referenced {
		return errors.New("image retention reference changed")
	}
	if _, err = tx.Exec(ctx, `DELETE FROM image_artifacts WHERE id=$1 AND account_id=$2`, item.ID, item.AccountID); err != nil {
		return storeError(err)
	}
	kind := "IMAGE_RETENTION_DELETE"
	if reason == "Student" {
		kind = "IMAGE_USER_DELETE"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,'system:image-gc',$3,'Succeeded',
            jsonb_build_object('artifactId',$4::text,'digest',$5::text))`,
		operationID, item.AccountID, kind, item.ID, item.Digest); err != nil {
		return storeError(err)
	}
	if reason == "Student" {
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='Succeeded',
            detail=detail || jsonb_build_object('deletedAt',clock_timestamp())
            WHERE account_id=$1 AND kind='IMAGE_DELETE_REQUEST' AND state='Queued'
              AND detail->>'artifactId'=$2`, item.AccountID, item.ID); err != nil {
			return storeError(err)
		}
	}
	return storeError(tx.Commit(ctx))
}
