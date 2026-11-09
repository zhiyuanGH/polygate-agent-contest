package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
	"polygate/internal/volume"
)

func (s *Store) ClaimNextVolume(ctx context.Context, workerID string, lease time.Duration) (volume.Task, bool, error) {
	var task volume.Task
	if workerID == "" || lease < 30*time.Second {
		return task, false, server.ErrUnavailable
	}
	err := s.Pool.QueryRow(ctx, `WITH next AS (
		SELECT v.id FROM student_volumes v JOIN accounts a ON a.id=v.account_id
		WHERE ((a.enabled AND a.volume_enabled AND v.state IN ('Reserved','Provisioning','Expanding'))
			OR v.state='Deleting')
		AND (v.lease_until IS NULL OR v.lease_until <= now())
		ORDER BY v.created_at,v.id FOR UPDATE OF v SKIP LOCKED LIMIT 1
	)
	UPDATE student_volumes v SET state=CASE WHEN v.state='Deleting' THEN 'Deleting' ELSE 'Provisioning' END,locked_by=$1,
		lease_until=now()+($2::bigint * interval '1 millisecond'),updated_at=now()
	FROM next WHERE v.id=next.id
	RETURNING v.id::text,v.account_id::text,v.size_gib,v.state,
		COALESCE(v.deletion_pvc_uid::text,''),COALESCE(v.deletion_pv_name,''),
		COALESCE(v.deletion_longhorn_handle,'')`, workerID, lease.Milliseconds()).Scan(
		&task.ID, &task.AccountID, &task.SizeGiB, &task.State, &task.PVCUID, &task.PVName, &task.Handle)
	if err == nil {
		return task, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	return task, false, storeError(err)
}

func (s *Store) RetryVolume(ctx context.Context, volumeID, workerID string, delay time.Duration) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE student_volumes SET locked_by=NULL,
		lease_until=now()+($3::bigint * interval '1 millisecond'),updated_at=now()
		WHERE id=$1 AND locked_by=$2 AND state IN ('Provisioning','Deleting') AND lease_until>now()`, volumeID, workerID, delay.Milliseconds())
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return server.ErrNotFound
	}
	return nil
}

func (s *Store) CompleteVolume(ctx context.Context, volumeID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var accountID string
	var sizeGiB int
	err = tx.QueryRow(ctx, `UPDATE student_volumes SET state='Ready',locked_by=NULL,
		lease_until=NULL,updated_at=now()
		WHERE id=$1 AND locked_by=$2 AND state='Provisioning' AND lease_until>now()
		RETURNING account_id::text,size_gib`, volumeID, workerID).Scan(&accountID, &sizeGiB)
	if err != nil {
		return storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE operations SET state='Succeeded'
		WHERE account_id=$1 AND kind='EXPAND_VOLUME' AND state='Pending'
		AND detail->>'volumeId'=$2 AND (detail->>'sizeGiB')::integer=$3`, accountID, volumeID, sizeGiB)
	if err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}

func (s *Store) RecordDeletionTarget(ctx context.Context, task volume.Task, workerID string) error {
	if task.PVCUID == "" || task.PVName == "" || task.Handle == "" {
		return server.ErrUnavailable
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE student_volumes SET deletion_pvc_uid=$3,
		deletion_pv_name=$4,deletion_longhorn_handle=$5,updated_at=now()
		WHERE id=$1 AND account_id=$6 AND locked_by=$2 AND state='Deleting' AND lease_until>now()
		AND ((deletion_pvc_uid IS NULL AND deletion_pv_name IS NULL AND deletion_longhorn_handle IS NULL)
		OR (deletion_pvc_uid=$3 AND deletion_pv_name=$4 AND deletion_longhorn_handle=$5))`,
		task.ID, workerID, task.PVCUID, task.PVName, task.Handle, task.AccountID)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return server.ErrNotFound
	}
	return nil
}

func (s *Store) CompleteDeletion(ctx context.Context, task volume.Task, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var accountID string
	err = tx.QueryRow(ctx, `DELETE FROM student_volumes WHERE id=$1 AND account_id=$2
		AND state='Deleting' AND locked_by=$3 AND lease_until>now()
		AND deletion_pvc_uid=$4 AND deletion_pv_name=$5 AND deletion_longhorn_handle=$6
		AND NOT EXISTS(SELECT 1 FROM apps WHERE volume_id=$1)
		RETURNING account_id::text`, task.ID, task.AccountID, workerID, task.PVCUID, task.PVName, task.Handle).Scan(&accountID)
	if err != nil {
		return storeError(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE operations SET state='Succeeded'
		WHERE account_id=$1 AND kind='DELETE_VOLUME' AND state='Pending'
		AND detail->>'volumeId'=$2`, accountID, task.ID)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return server.ErrUnavailable
	}
	return storeError(tx.Commit(ctx))
}
