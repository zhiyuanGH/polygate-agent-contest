package postgres

import "context"

// DeploymentQueueHealthy detects a lease that was not reclaimed promptly or
// an active deploy, update, rollback, or stop task stranded for 30 minutes.
func (s *Store) DeploymentQueueHealthy(ctx context.Context) (bool, error) {
	var stale bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_tasks
		WHERE (state='Leased' AND (lease_until IS NULL OR lease_until < now()-interval '2 minutes'))
		   OR (state IN ('Queued','Leased') AND created_at < now()-interval '30 minutes'))`).Scan(&stale)
	return !stale, storeError(err)
}
