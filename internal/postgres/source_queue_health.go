package postgres

import "context"

// SourceQueueHealthy reports whether an enabled account has a source build
// lease that expired over two minutes ago without being reclaimed.
func (s *Store) SourceQueueHealthy(ctx context.Context) (bool, error) {
	var expired int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM source_build_tasks t
		JOIN accounts a ON a.id=t.account_id
		WHERE t.state='Leased' AND a.enabled AND a.source_build_enabled
		AND t.lease_until < clock_timestamp() - interval '2 minutes'`).Scan(&expired)
	return expired == 0, storeError(err)
}
