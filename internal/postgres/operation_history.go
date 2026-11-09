package postgres

import (
	"context"

	"polygate/internal/server"
)

func (s *Store) ListOperationHistory(ctx context.Context, accountID, before string) (server.OperationPage, error) {
	rows, err := s.Pool.Query(ctx, `SELECT o.id::text,COALESCE(o.app_id::text,''),o.kind,o.state,o.detail,o.created_at
		FROM operations o WHERE o.account_id=$1
		AND ($2='' OR (o.created_at,o.id) < (
			SELECT cursor.created_at,cursor.id FROM operations cursor
			WHERE cursor.account_id=$1 AND cursor.id=NULLIF($2,'')::uuid))
		ORDER BY o.created_at DESC,o.id DESC LIMIT 51`, accountID, before)
	if err != nil {
		return server.OperationPage{}, storeError(err)
	}
	defer rows.Close()
	page := server.OperationPage{Items: []server.Operation{}}
	for rows.Next() {
		var op server.Operation
		if err := rows.Scan(&op.ID, &op.AppID, &op.Kind, &op.State, &op.Detail, &op.CreatedAt); err != nil {
			return server.OperationPage{}, storeError(err)
		}
		page.Items = append(page.Items, op)
	}
	if err := rows.Err(); err != nil {
		return server.OperationPage{}, storeError(err)
	}
	if len(page.Items) > 50 {
		page.Items = page.Items[:50]
		page.NextCursor = page.Items[49].ID
	}
	return page, nil
}
