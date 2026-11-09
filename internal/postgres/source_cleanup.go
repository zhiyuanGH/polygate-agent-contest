package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListPreparingSourceBuilds(ctx context.Context, olderThan time.Duration) ([]SourceBuildTask, error) {
	if olderThan < time.Minute || olderThan > 24*time.Hour {
		return nil, errors.New("invalid source recovery age")
	}
	rows, err := s.Pool.Query(ctx, `SELECT t.id::text,t.artifact_id::text,t.account_id::text,a.size_bytes,t.created_at
		FROM source_build_tasks t JOIN source_artifacts a ON a.id=t.artifact_id AND a.account_id=t.account_id
		WHERE t.state='Preparing' AND t.created_at<clock_timestamp()-($1::bigint * interval '1 millisecond')
		ORDER BY t.created_at,t.id LIMIT 100`, olderThan.Milliseconds())
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]SourceBuildTask, 0)
	for rows.Next() {
		var task SourceBuildTask
		if err = rows.Scan(&task.ID, &task.ArtifactID, &task.AccountID, &task.SizeBytes, &task.CreatedAt); err != nil {
			return nil, storeError(err)
		}
		items = append(items, task)
	}
	return items, storeError(rows.Err())
}

func (s *Store) SourceArtifactReferenced(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM source_artifacts WHERE id=$1)`, id).Scan(&exists)
	return exists, storeError(err)
}

func (s *Store) SourceBuildReferenced(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM source_build_tasks WHERE id=$1)`, id).Scan(&exists)
	return exists, storeError(err)
}

// ExpireSourceBuilds removes abandoned pre-upload reservations after 24 hours
// and completed/failed builds after the 14-day source retention period.
// The caller removes their files only after this transaction commits.
type ExpiredSourceBuild struct {
	TaskID     string
	ArtifactID string
}

func (s *Store) ExpireSourceBuilds(ctx context.Context) ([]ExpiredSourceBuild, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, storeError(err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `DELETE FROM source_build_tasks
		WHERE (state='Preparing' AND created_at<clock_timestamp()-interval '24 hours')
		OR (state IN ('Succeeded','Failed') AND updated_at<clock_timestamp()-interval '14 days')
		RETURNING id::text,artifact_id::text`)
	if err != nil {
		return nil, storeError(err)
	}
	ids := make([]string, 0)
	expired := make([]ExpiredSourceBuild, 0)
	taskIDs := make([]string, 0)
	for rows.Next() {
		var taskID, id string
		if err = rows.Scan(&taskID, &id); err != nil {
			rows.Close()
			return nil, storeError(err)
		}
		ids = append(ids, id)
		expired = append(expired, ExpiredSourceBuild{TaskID: taskID, ArtifactID: id})
		taskIDs = append(taskIDs, taskID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, storeError(err)
	}
	if len(ids) > 0 {
		_, err = tx.Exec(ctx, `UPDATE operations SET state='Failed',
			detail=detail || jsonb_build_object('errorCode','SOURCE_UPLOAD_EXPIRED')
			WHERE kind='BUILD_SOURCE' AND state='Preparing'
			AND detail->>'taskId'=ANY($1::text[])`, taskIDs)
		if err != nil {
			return nil, storeError(err)
		}
		_, err = tx.Exec(ctx, `DELETE FROM source_artifacts WHERE id=ANY($1::uuid[])`, ids)
		if err != nil {
			return nil, storeError(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, storeError(err)
	}
	return expired, nil
}
