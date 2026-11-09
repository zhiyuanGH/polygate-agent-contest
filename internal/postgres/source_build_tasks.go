package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/server"
)

const maxSourceBuildAttempts = 3
const maxSourceArchiveReservation int64 = 40 << 30 // Leave 10GiB of the 50GiB PVC for build logs and filesystem headroom.

var sourceSHA256 = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type SourceBuildTask = server.SourceBuildTask

// ReserveSourceBuild holds quota before the API writes the archive. A crashed
// upload leaves a nonclaimable Preparing task for the cleanup job to expire.
func (s *Store) ReserveSourceBuild(ctx context.Context, accountID, actor, artifactID string, size int64) (SourceBuildTask, error) {
	if size < 1 || size > 100<<20 || strings.TrimSpace(actor) == "" || len(actor) > 512 {
		return SourceBuildTask{}, &domain.Error{Code: "INVALID_SOURCE", Message: "Invalid source archive metadata."}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	defer tx.Rollback(ctx)
	// Concurrent accounts must not both reserve the last bytes of the 50GiB
	// shared source volume. This lock is independent of the two build slots.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7071, 2)`); err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	var enabled, buildEnabled bool
	err = tx.QueryRow(ctx, `SELECT enabled,source_build_enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled, &buildEnabled)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if !enabled {
		return SourceBuildTask{}, server.ErrDisabled
	}
	if !buildEnabled {
		return SourceBuildTask{}, &domain.Error{Code: "BUILD_NOT_ENABLED", Message: "Source builds are not enabled for this account."}
	}
	var waiting int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM source_build_tasks
		WHERE account_id=$1 AND state IN ('Preparing','Queued')`, accountID).Scan(&waiting)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if waiting >= 3 {
		return SourceBuildTask{}, &domain.Error{Code: "BUILD_QUEUE_FULL", Message: "This account already has three waiting builds."}
	}
	var accountBytes, globalBytes int64
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(size_bytes),0) FROM source_artifacts
		WHERE account_id=$1`, accountID).Scan(&accountBytes)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(size_bytes),0) FROM source_artifacts`).Scan(&globalBytes)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if accountBytes > (1<<30)-size || globalBytes > maxSourceArchiveReservation-size {
		return SourceBuildTask{}, &domain.Error{Code: "SOURCE_STORAGE_FULL", Message: "Source storage quota is full."}
	}
	taskID, err := server.NewID()
	if err != nil {
		return SourceBuildTask{}, err
	}
	opID, err := server.NewID()
	if err != nil {
		return SourceBuildTask{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_artifacts(id,account_id,size_bytes)
		VALUES($1,$2,$3)`, artifactID, accountID, size)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO source_build_tasks(id,artifact_id,account_id)
		VALUES($1,$2,$3) RETURNING created_at`, taskID, artifactID, accountID).Scan(&createdAt)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,'BUILD_SOURCE','Preparing',jsonb_build_object('taskId',$4::text))`,
		opID, accountID, actor, taskID)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	return SourceBuildTask{ID: taskID, ArtifactID: artifactID, AccountID: accountID,
		SizeBytes: size, State: "Preparing", CreatedAt: createdAt}, nil
}

func (s *Store) ActivateSourceBuild(ctx context.Context, accountID, artifactID, digest string) (SourceBuildTask, error) {
	if !sourceSHA256.MatchString(digest) {
		return SourceBuildTask{}, errors.New("invalid source digest")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, buildEnabled bool
	if err = tx.QueryRow(ctx, `SELECT enabled,source_build_enabled FROM accounts
		WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled, &buildEnabled); err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if !enabled {
		return SourceBuildTask{}, server.ErrDisabled
	}
	if !buildEnabled {
		return SourceBuildTask{}, &domain.Error{Code: "BUILD_NOT_ENABLED", Message: "Source builds are not enabled for this account."}
	}
	var task SourceBuildTask
	err = tx.QueryRow(ctx, `SELECT t.id::text,t.account_id::text,a.size_bytes,t.created_at
		FROM source_build_tasks t JOIN source_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
		WHERE t.artifact_id=$1 AND t.account_id=$2 AND t.state='Preparing' AND a.sha256 IS NULL
		FOR UPDATE OF t,a`, artifactID, accountID).Scan(&task.ID, &task.AccountID, &task.SizeBytes, &task.CreatedAt)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE source_artifacts SET sha256=$3 WHERE id=$1 AND account_id=$2`, artifactID, accountID, digest)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE source_build_tasks SET state='Queued',updated_at=now()
		WHERE id=$1 AND account_id=$2`, task.ID, accountID)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE operations SET state='Queued',
		detail=detail || jsonb_build_object('sourceSha256',$3::text)
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND state='Preparing'
		AND detail->>'taskId'=$2`, accountID, task.ID, digest)
	if err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	if result.RowsAffected() != 1 {
		return SourceBuildTask{}, errors.New("source build operation missing")
	}
	if err = tx.Commit(ctx); err != nil {
		return SourceBuildTask{}, storeError(err)
	}
	task.ArtifactID = artifactID
	task.SourceSHA256 = digest
	task.State = "Queued"
	return task, nil
}

func (s *Store) AbandonSourceBuild(ctx context.Context, accountID, artifactID string) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var taskID string
	err = tx.QueryRow(ctx, `DELETE FROM source_build_tasks WHERE account_id=$1 AND artifact_id=$2
		AND state='Preparing' RETURNING id::text`, accountID, artifactID).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, storeError(err)
	}
	_, err = tx.Exec(ctx, `DELETE FROM source_artifacts WHERE id=$1 AND account_id=$2`, artifactID, accountID)
	if err != nil {
		return false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE operations SET state='Failed',
		detail=detail || jsonb_build_object('errorCode','SOURCE_STORAGE_FAILED')
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND state='Preparing' AND detail->>'taskId'=$2`, accountID, taskID)
	if err != nil {
		return false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, storeError(err)
	}
	return true, nil
}

func (s *Store) ListSourceBuilds(ctx context.Context, accountID string) ([]SourceBuildTask, error) {
	rows, err := s.Pool.Query(ctx, `SELECT t.id::text,t.artifact_id::text,coalesce(a.sha256,''),a.size_bytes,
		t.state,t.attempt,coalesce(t.error_code,''),coalesce(t.result_image_artifact_id::text,''),
		t.result_image_deleted_at,t.created_at
		FROM source_build_tasks t JOIN source_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
		WHERE t.account_id=$1 ORDER BY t.created_at DESC,t.id DESC LIMIT 50`, accountID)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]SourceBuildTask, 0)
	for rows.Next() {
		var task SourceBuildTask
		err = rows.Scan(&task.ID, &task.ArtifactID, &task.SourceSHA256, &task.SizeBytes,
			&task.State, &task.Attempt, &task.ErrorCode, &task.ResultImageArtifactID,
			&task.ResultImageDeletedAt, &task.CreatedAt)
		if err != nil {
			return nil, storeError(err)
		}
		items = append(items, task)
	}
	return items, storeError(rows.Err())
}

func (s *Store) GetSourceBuild(ctx context.Context, accountID, taskID string) (SourceBuildTask, error) {
	var task SourceBuildTask
	err := s.Pool.QueryRow(ctx, `SELECT t.id::text,t.artifact_id::text,coalesce(a.sha256,''),a.size_bytes,
		t.state,t.attempt,coalesce(t.error_code,''),coalesce(t.result_image_artifact_id::text,''),
		t.result_image_deleted_at,t.created_at
		FROM source_build_tasks t JOIN source_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
		WHERE t.account_id=$1 AND t.id=$2`, accountID, taskID).Scan(
		&task.ID, &task.ArtifactID, &task.SourceSHA256, &task.SizeBytes,
		&task.State, &task.Attempt, &task.ErrorCode, &task.ResultImageArtifactID,
		&task.ResultImageDeletedAt, &task.CreatedAt)
	return task, storeError(err)
}

// ClaimNextSourceBuild serializes the global two-slot decision in PostgreSQL.
// An expired lease is eligible again; the attempt number fences its old owner.
func (s *Store) ClaimNextSourceBuild(ctx context.Context, workerID string, lease time.Duration) (SourceBuildTask, bool, error) {
	if workerID == "" || strings.TrimSpace(workerID) != workerID || len(workerID) > 128 ||
		lease < time.Minute || lease > 30*time.Minute {
		return SourceBuildTask{}, false, errors.New("invalid build worker identity or lease")
	}
	for {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return SourceBuildTask{}, false, storeError(err)
		}
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7071, 1)`)
		if err != nil {
			_ = tx.Rollback(ctx)
			return SourceBuildTask{}, false, storeError(err)
		}
		var active int
		err = tx.QueryRow(ctx, `SELECT count(*) FROM source_build_tasks
			WHERE state='Leased' AND lease_until>clock_timestamp()`).Scan(&active)
		if err != nil {
			_ = tx.Rollback(ctx)
			return SourceBuildTask{}, false, storeError(err)
		}
		if active >= 2 {
			_ = tx.Rollback(ctx)
			return SourceBuildTask{}, false, nil
		}
		var task SourceBuildTask
		err = tx.QueryRow(ctx, `SELECT t.id::text,t.artifact_id::text,t.account_id::text,
			a.sha256,a.size_bytes,t.attempt
			FROM source_build_tasks t JOIN source_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
			JOIN accounts owner ON owner.id=t.account_id AND owner.enabled AND owner.source_build_enabled
			AND a.sha256 IS NOT NULL
			WHERE ((t.state='Queued' AND t.next_attempt_at<=clock_timestamp()) OR
				(t.state='Leased' AND t.lease_until<=clock_timestamp()))
			AND NOT EXISTS (SELECT 1 FROM source_build_tasks other WHERE other.account_id=t.account_id
				AND other.id<>t.id AND other.state='Leased' AND other.lease_until>clock_timestamp())
			ORDER BY t.created_at,t.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).Scan(
			&task.ID, &task.ArtifactID, &task.AccountID, &task.SourceSHA256, &task.SizeBytes, &task.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return SourceBuildTask{}, false, nil
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return SourceBuildTask{}, false, storeError(err)
		}
		if task.Attempt >= maxSourceBuildAttempts {
			_, err = tx.Exec(ctx, `UPDATE source_build_tasks SET state='Failed',locked_by=NULL,
				lease_until=NULL,error_code='BUILD_ATTEMPTS_EXHAUSTED',updated_at=now() WHERE id=$1`, task.ID)
			if err == nil {
				var opResult pgconn.CommandTag
				opResult, err = tx.Exec(ctx, `UPDATE operations SET state='Failed',
					detail=detail || jsonb_build_object('errorCode','BUILD_ATTEMPTS_EXHAUSTED')
					WHERE account_id=$1 AND kind='BUILD_SOURCE' AND detail->>'taskId'=$2`,
					task.AccountID, task.ID)
				if err == nil && opResult.RowsAffected() != 1 {
					err = errors.New("source build operation missing")
				}
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			if err != nil {
				return SourceBuildTask{}, false, storeError(err)
			}
			continue
		}
		err = tx.QueryRow(ctx, `UPDATE source_build_tasks SET state='Leased',attempt=attempt+1,
			locked_by=$2,lease_until=clock_timestamp()+($3::bigint * interval '1 millisecond'),
			error_code=NULL,updated_at=now() WHERE id=$1 RETURNING attempt`,
			task.ID, workerID, lease.Milliseconds()).Scan(&task.Attempt)
		if err == nil {
			var opResult pgconn.CommandTag
			opResult, err = tx.Exec(ctx, `UPDATE operations SET state='Building',
				detail=detail - 'errorCode' WHERE account_id=$1 AND kind='BUILD_SOURCE'
				AND detail->>'taskId'=$2`, task.AccountID, task.ID)
			if err == nil && opResult.RowsAffected() != 1 {
				err = errors.New("source build operation missing")
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return SourceBuildTask{}, false, storeError(err)
		}
		task.State = "Leased"
		return task, true, nil
	}
}

func (s *Store) FailSourceBuild(ctx context.Context, taskID, workerID string, attempt int, code string, retry bool) error {
	if !imageScanErrorCode.MatchString(code) || attempt < 1 || attempt > maxSourceBuildAttempts {
		return errors.New("invalid build failure")
	}
	state := "Failed"
	if retry && attempt < maxSourceBuildAttempts {
		state = "Queued"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var accountID string
	err = tx.QueryRow(ctx, `UPDATE source_build_tasks SET state=$4,locked_by=NULL,
		lease_until=NULL,error_code=$5,next_attempt_at=clock_timestamp()+interval '15 seconds',updated_at=now()
		WHERE id=$1 AND locked_by=$2 AND attempt=$3 AND state='Leased' AND lease_until>clock_timestamp()
		RETURNING account_id::text`, taskID, workerID, attempt, state, code).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrNotFound
	}
	if err != nil {
		return storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE operations SET state=$3,
		detail=detail || jsonb_build_object('errorCode',$4::text)
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND detail->>'taskId'=$2`,
		accountID, taskID, state, code)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("source build operation missing")
	}
	return storeError(tx.Commit(ctx))
}

// A build only succeeds after its result image has been registered under the
// same account. The foreign key rejects cross-account image substitution.
func (s *Store) CompleteSourceBuild(ctx context.Context, taskID, workerID string, attempt int, imageArtifactID string) error {
	if attempt < 1 || attempt > maxSourceBuildAttempts {
		return errors.New("invalid build attempt")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var accountID string
	err = tx.QueryRow(ctx, `UPDATE source_build_tasks SET state='Succeeded',locked_by=NULL,
		lease_until=NULL,error_code=NULL,result_image_artifact_id=$4,updated_at=now()
		WHERE id=$1 AND locked_by=$2 AND attempt=$3 AND state='Leased' AND lease_until>clock_timestamp()
		RETURNING account_id::text`, taskID, workerID, attempt, imageArtifactID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrNotFound
	}
	if err != nil {
		return storeError(err)
	}
	result, err := tx.Exec(ctx, `UPDATE operations SET state='Succeeded',
		detail=(detail - 'errorCode') || jsonb_build_object('imageArtifactId',$3::text)
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND detail->>'taskId'=$2`,
		accountID, taskID, imageArtifactID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("source build operation missing")
	}
	return storeError(tx.Commit(ctx))
}
