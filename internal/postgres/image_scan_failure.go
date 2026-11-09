package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

func (s *Store) GetImageScanFailure(ctx context.Context, accountID, artifactID string) (server.ImageScanFailure, error) {
	var failure server.ImageScanFailure
	err := s.Pool.QueryRow(ctx, `SELECT a.id::text,a.scan_status,t.error_code,t.attempt,t.updated_at
		FROM image_artifacts a JOIN image_scan_tasks t
		ON t.artifact_id=a.id AND t.account_id=a.account_id
		WHERE a.id=$1 AND a.account_id=$2 AND a.gc_state='Active'
		AND a.scan_status='Failed' AND t.state='Failed'`, artifactID, accountID).Scan(
		&failure.ArtifactID, &failure.Status, &failure.ErrorCode, &failure.Attempts, &failure.CheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return failure, server.ErrNotFound
	}
	if err != nil {
		return failure, storeError(err)
	}
	return failure, nil
}
