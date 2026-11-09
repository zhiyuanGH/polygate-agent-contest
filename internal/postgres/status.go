package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

func (s *Store) DeploymentStatus(ctx context.Context, accountID, appID string) (server.DeploymentStatus, error) {
	var status server.DeploymentStatus
	err := s.Pool.QueryRow(ctx, `SELECT id::text,status FROM apps
        WHERE account_id=$1 AND id=$2`, accountID, appID).Scan(&status.AppID, &status.Status)
	if err != nil {
		return server.DeploymentStatus{}, storeError(err)
	}
	err = s.Pool.QueryRow(ctx, `SELECT id::text,state,attempt,COALESCE(error_code,'')
        FROM deployment_tasks WHERE account_id=$1 AND app_id=$2
        ORDER BY created_at DESC,id DESC LIMIT 1`, accountID, appID).Scan(
		&status.TaskID, &status.TaskState, &status.Attempt, &status.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return server.DeploymentStatus{}, storeError(err)
	}
	return status, nil
}
