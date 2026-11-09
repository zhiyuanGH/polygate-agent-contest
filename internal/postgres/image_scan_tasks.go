package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"polygate/internal/deploy"
	"polygate/internal/server"
)

const maxImageScanAttempts = 8

var imageScanErrorCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ImageScanTask contains only the immutable source reference and the account
// that owns it. Registry credentials are supplied separately by the worker.
type ImageScanTask struct {
	ArtifactID      string
	AccountID       string
	PinnedReference string
	PlatformDigest  string
	RegistryHost    string
	ScanStatus      string
	Attempt         int
}

// ClaimNextImageScan leases one pending image. Expired leases are recoverable;
// exhausted tasks are marked failed before another task is considered.
func (s *Store) ClaimNextImageScan(ctx context.Context, workerID string, lease time.Duration) (ImageScanTask, bool, error) {
	if strings.TrimSpace(workerID) != workerID || len(workerID) == 0 || len(workerID) > 128 ||
		lease < 30*time.Second || lease > 15*time.Minute {
		return ImageScanTask{}, false, errors.New("invalid image scan worker identity or lease")
	}
	for {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return ImageScanTask{}, false, storeError(err)
		}
		var task ImageScanTask
		err = tx.QueryRow(ctx, `SELECT t.artifact_id::text,t.account_id::text,
            a.pinned_reference,a.platform_digest,a.registry_host,a.scan_status,t.attempt
            FROM image_scan_tasks t
            JOIN image_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
            WHERE ((t.state='Queued' AND t.next_attempt_at<=clock_timestamp()) OR
               (t.state='Leased' AND t.lease_until<=clock_timestamp()))
            ORDER BY t.created_at,t.artifact_id LIMIT 1
            FOR UPDATE OF t SKIP LOCKED`).Scan(&task.ArtifactID, &task.AccountID,
			&task.PinnedReference, &task.PlatformDigest, &task.RegistryHost,
			&task.ScanStatus, &task.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return ImageScanTask{}, false, nil
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return ImageScanTask{}, false, storeError(err)
		}
		if task.ScanStatus == "Approved" || task.ScanStatus == "Rejected" {
			_, err = tx.Exec(ctx, `UPDATE image_scan_tasks SET state='Succeeded',
                locked_by=NULL,lease_until=NULL,error_code=NULL,updated_at=now()
                WHERE artifact_id=$1`, task.ArtifactID)
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			if err != nil {
				return ImageScanTask{}, false, storeError(err)
			}
			continue
		}
		if task.Attempt >= maxImageScanAttempts {
			_, err = tx.Exec(ctx, `UPDATE image_scan_tasks SET state='Failed',
                locked_by=NULL,lease_until=NULL,error_code='IMAGE_SCAN_ATTEMPTS_EXHAUSTED',
                updated_at=now() WHERE artifact_id=$1`, task.ArtifactID)
			if err == nil {
				err = finishImageScanFailureTx(ctx, tx, task.AccountID, task.ArtifactID,
					"IMAGE_SCAN_ATTEMPTS_EXHAUSTED")
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			if err != nil {
				return ImageScanTask{}, false, storeError(err)
			}
			continue
		}
		err = tx.QueryRow(ctx, `UPDATE image_scan_tasks SET state='Leased',locked_by=$2,
            lease_until=clock_timestamp()+($3::bigint * interval '1 millisecond'),
            attempt=attempt+1,error_code=NULL,updated_at=now()
            WHERE artifact_id=$1 RETURNING attempt`, task.ArtifactID, workerID,
			lease.Milliseconds()).Scan(&task.Attempt)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return ImageScanTask{}, false, storeError(err)
		}
		return task, true, nil
	}
}

// RetryImageScan releases a live lease for a later attempt. The eighth failed
// attempt is terminal, including after a worker crash and lease expiry.
func (s *Store) RetryImageScan(ctx context.Context, artifactID, workerID, code string, delay time.Duration) error {
	if !imageScanErrorCode.MatchString(code) || delay < 0 || delay > time.Hour {
		return errors.New("invalid image scan retry parameters")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var attempt int
	var accountID string
	err = tx.QueryRow(ctx, `SELECT attempt,account_id::text FROM image_scan_tasks
        WHERE artifact_id=$1 AND state='Leased' AND locked_by=$2 AND lease_until>clock_timestamp()
        FOR UPDATE`, artifactID, workerID).Scan(&attempt, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.ErrLeaseLost
	}
	if err != nil {
		return storeError(err)
	}
	state := "Queued"
	if attempt >= maxImageScanAttempts {
		state = "Failed"
	}
	_, err = tx.Exec(ctx, `UPDATE image_scan_tasks SET state=$3,locked_by=NULL,
        lease_until=NULL,next_attempt_at=clock_timestamp()+($5::bigint * interval '1 millisecond'),
        error_code=$4,updated_at=now() WHERE artifact_id=$1 AND locked_by=$2`,
		artifactID, workerID, state, code, delay.Milliseconds())
	if err != nil {
		return storeError(err)
	}
	if state == "Failed" {
		if err := finishImageScanFailureTx(ctx, tx, accountID, artifactID, code); err != nil {
			return err
		}
	}
	return storeError(tx.Commit(ctx))
}

// FailImageScan ends a task on a non-retryable failure, such as a changed
// source digest or a forbidden registry redirect.
func (s *Store) FailImageScan(ctx context.Context, artifactID, workerID, code string) error {
	if !imageScanErrorCode.MatchString(code) {
		return errors.New("invalid image scan failure code")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var accountID string
	err = tx.QueryRow(ctx, `UPDATE image_scan_tasks SET state='Failed',
        locked_by=NULL,lease_until=NULL,error_code=$3,updated_at=now()
		WHERE artifact_id=$1 AND state='Leased' AND locked_by=$2 AND lease_until>clock_timestamp()
		RETURNING account_id::text`, artifactID, workerID, code).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploy.ErrLeaseLost
	}
	if err != nil {
		return storeError(err)
	}
	if err = finishImageScanFailureTx(ctx, tx, accountID, artifactID, code); err != nil {
		return err
	}
	return storeError(tx.Commit(ctx))
}

func finishImageScanFailureTx(ctx context.Context, tx pgx.Tx,
	accountID, artifactID, code string) error {
	result, err := tx.Exec(ctx, `UPDATE image_artifacts SET scan_status='Failed'
        WHERE id=$1 AND account_id=$2 AND scan_status='PendingScan'`, artifactID, accountID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("failed image scan has no pending artifact")
	}
	_, err = tx.Exec(ctx, `UPDATE operations
        SET state='Failed',detail=jsonb_set(detail,'{errorCode}',to_jsonb($3::text),true)
        WHERE account_id=$1 AND kind='IMAGE_INSPECT' AND state='PendingScan'
          AND detail->>'artifactId'=$2`, accountID, artifactID, code)
	if err != nil {
		return storeError(err)
	}
	opID, err := server.NewID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,'system:harbor-scanner','IMAGE_SCAN','Failed',
            jsonb_build_object('artifactId',$3::text,'errorCode',$4::text))`,
		opID, accountID, artifactID, code)
	return storeError(err)
}

// CompleteImageScanTask acknowledges only a completed, evidence-backed scan.
// RecordImageScan is idempotent, so a crash between evidence commit and this
// acknowledgement can be replayed safely by the next lease holder.
func (s *Store) CompleteImageScanTask(ctx context.Context, artifactID, workerID string) error {
	result, err := s.Pool.Exec(ctx, `UPDATE image_scan_tasks t SET state='Succeeded',
        locked_by=NULL,lease_until=NULL,error_code=NULL,updated_at=now()
        FROM image_artifacts a
        WHERE t.artifact_id=$1 AND t.state='Leased' AND t.locked_by=$2
          AND t.lease_until>clock_timestamp() AND a.id=t.artifact_id
          AND a.account_id=t.account_id AND a.scan_status IN ('Approved','Rejected')
          AND a.harbor_reference IS NOT NULL AND a.scan_report_sha256 IS NOT NULL`,
		artifactID, workerID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return deploy.ErrLeaseLost
	}
	return nil
}
