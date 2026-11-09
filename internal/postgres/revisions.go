package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"polygate/internal/domain"
	"polygate/internal/server"
)

// ListRevisions returns only the caller's app history. A cursor is the next
// older revision number, so inserts cannot shift an in-progress page walk.
func (s *Store) ListRevisions(ctx context.Context, accountID, appID string, before, limit int) (server.RevisionPage, error) {
	if _, err := uuid.Parse(appID); err != nil {
		return server.RevisionPage{}, server.ErrNotFound
	}
	if before < 0 || limit < 1 || limit > 100 {
		return server.RevisionPage{}, &domain.Error{Code: "INVALID_REVISION_PAGE", Message: "The revision page is invalid."}
	}
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE account_id=$1 AND id=$2)`, accountID, appID).Scan(&exists)
	if err != nil {
		return server.RevisionPage{}, storeError(err)
	}
	if !exists {
		return server.RevisionPage{}, server.ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT r.id::text,r.revision,r.platform_image_digest,r.spec,
		r.source_kind,COALESCE(r.image_artifact_id::text,''),
		EXISTS(SELECT 1 FROM deployment_tasks t WHERE t.revision_id=r.id
		 AND t.account_id=$1 AND t.app_id=$2 AND t.state='Succeeded' AND t.task_kind IN ('Deploy','Update')),
		EXISTS(SELECT 1 FROM resource_reservations rr WHERE rr.revision_id=r.id AND rr.account_id=$1 AND rr.app_id=$2)
		 AND EXISTS(SELECT 1 FROM deployment_tasks t WHERE t.revision_id=r.id
		 AND t.account_id=$1 AND t.app_id=$2 AND t.state='Succeeded' AND t.task_kind IN ('Deploy','Update')),
		r.created_at
		FROM app_revisions r WHERE r.account_id=$1 AND r.app_id=$2
		AND ($3::integer=0 OR r.revision<$3)
		ORDER BY revision DESC LIMIT $4`, accountID, appID, before, limit+1)
	if err != nil {
		return server.RevisionPage{}, storeError(err)
	}
	defer rows.Close()
	page := server.RevisionPage{Items: make([]server.Revision, 0, limit)}
	for rows.Next() {
		var item server.Revision
		var raw []byte
		if err := rows.Scan(&item.ID, &item.Number, &item.ImageDigest, &raw,
			&item.SourceKind, &item.ImageArtifactID, &item.Successful, &item.Current, &item.CreatedAt); err != nil {
			return server.RevisionPage{}, storeError(err)
		}
		if err := json.Unmarshal(raw, &item.Spec); err != nil {
			return server.RevisionPage{}, storeError(err)
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return server.RevisionPage{}, storeError(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextBeforeRevision = page.Items[len(page.Items)-1].Number
	}
	return page, nil
}
