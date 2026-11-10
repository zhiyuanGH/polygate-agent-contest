package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"polygate/internal/deploy"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func imageRetryIntegrationStore(t *testing.T) (*Store, context.Context, string) {
	t.Helper()
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to an isolated migrated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	s, err := Open(ctx, url)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	account, err := server.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
		VALUES($1,$2,$2,'Image retry integration')`, account, "retry-test-"+account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = s.Pool.Exec(cleanup, `DELETE FROM operations WHERE account_id=$1`, account)
		_, _ = s.Pool.Exec(cleanup, `DELETE FROM image_artifacts WHERE account_id=$1`, account)
		_, _ = s.Pool.Exec(cleanup, `DELETE FROM accounts WHERE id=$1`, account)
		s.Close()
		cancel()
	})
	return s, ctx, account
}

func imageRetryFixture(t *testing.T, s *Store, ctx context.Context, account string) server.ImageArtifact {
	t.Helper()
	unique, err := server.NewID()
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat(strings.ReplaceAll(unique, "-", ""), 2)
	image := registry.Image{OS: "linux", Architecture: "amd64", RegistryHost: "registry.example",
		SubmittedReference: "registry.example/team/agent:v1", PinnedReference: "registry.example/team/agent@" + digest,
		ManifestDigest: digest, PlatformDigest: digest}
	item, _, err := s.SaveImageArtifact(ctx, account, "original-actor", image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE image_scan_tasks SET state='Failed',attempt=8,error_code='IMAGE_SCAN_UNAVAILABLE'
		WHERE artifact_id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE image_artifacts SET scan_status='Failed' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE operations SET state='Failed',detail=detail||'{"errorCode":"IMAGE_SCAN_UNAVAILABLE"}'::jsonb
		WHERE account_id=$1 AND kind='IMAGE_INSPECT' AND detail->>'artifactId'=$2`, account, item.ID); err != nil {
		t.Fatal(err)
	}
	return item
}

func retryScanEvidence(digest string) harbor.Evidence {
	report := []byte(`{"vulnerabilities":[]}`)
	sum := sha256.Sum256(report)
	return harbor.Evidence{Digest: digest, ReportID: "retry-report", ReportSHA256: "sha256:" + hex.EncodeToString(sum[:]),
		ScannerName: "Trivy", ScannedAt: time.Now().UTC(), Report: report, Severities: map[string]int{}}
}

func TestImageScanRetryRequeuesAndFencesLeases(t *testing.T) {
	s, ctx, account := imageRetryIntegrationStore(t)
	before := imageRetryFixture(t, s, ctx, account)
	queued, replay, err := s.QueueImageScanRetry(ctx, account, "retry-actor", before.ID, "retry-owner-key")
	if err != nil || replay || queued.ID != before.ID || queued.ScanStatus != "PendingScan" || queued.ScanTaskState != "Queued" || queued.ScanErrorCode != "" || queued.ScanSummary != nil {
		t.Fatalf("retry did not queue cleanly: %+v replay=%v err=%v", queued, replay, err)
	}
	if queued.PinnedReference != before.PinnedReference || queued.PlatformDigest != before.PlatformDigest || !queued.CreatedAt.Equal(before.CreatedAt) {
		t.Fatal("retry changed immutable image identity or creation time")
	}
	var attempts, audits int
	var actor, state, previousCode string
	if err := s.Pool.QueryRow(ctx, `SELECT attempt FROM image_scan_tasks WHERE artifact_id=$1`, before.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempt budget not reset: %d %v", attempts, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_SCAN_RETRY'`, account).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("retry audit count=%d err=%v", audits, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT actor_subject,state,detail->>'previousErrorCode'
		FROM operations WHERE account_id=$1 AND kind='IMAGE_SCAN_RETRY'`, account).Scan(&actor, &state, &previousCode); err != nil || actor != "retry-actor" || state != "Succeeded" || previousCode != "IMAGE_SCAN_UNAVAILABLE" {
		t.Fatalf("retry audit lost actor or previous failure: %s %s %s %v", actor, state, previousCode, err)
	}
	if err := s.FailImageScan(ctx, before.ID, "old-worker", "IMAGE_SCAN_UNAVAILABLE"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("old lease changed retried task: %v", err)
	}
	claimed, ok, err := s.ClaimNextImageScan(ctx, "new-worker", time.Minute)
	if err != nil || !ok || claimed.ArtifactID != before.ID || claimed.Attempt != 1 {
		t.Fatalf("existing worker cannot claim retry: %+v ok=%v err=%v", claimed, ok, err)
	}
	if item, replay, err := s.QueueImageScanRetry(ctx, account, "retry-actor", before.ID, "retry-owner-key"); err != nil || !replay || item.ScanTaskState != "Leased" {
		t.Fatalf("same key reset or rejected running retry: %+v %v %v", item, replay, err)
	}
	if _, _, err := s.QueueImageScanRetry(ctx, account, "retry-actor", before.ID, "new-during-lease"); !errors.Is(err, server.ErrImageScanNotRetryable) {
		t.Fatalf("new retry admitted during scan: %v", err)
	}
	ref := harbor.RegistryHost + "/pg-u-" + strings.ReplaceAll(account, "-", "") + "/agent@" + claimed.PlatformDigest
	if _, _, err := s.RecordLeasedImageScan(ctx, account, before.ID, "old-worker", ref, retryScanEvidence(claimed.PlatformDigest)); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("old worker committed retry evidence: %v", err)
	}
	decision, replay, err := s.RecordLeasedImageScan(ctx, account, before.ID, "new-worker", ref, retryScanEvidence(claimed.PlatformDigest))
	if err != nil || replay || decision != "Approved" {
		t.Fatalf("retry scan did not complete: %s %v %v", decision, replay, err)
	}
	if item, replay, err := s.QueueImageScanRetry(ctx, account, "retry-actor", before.ID, "retry-owner-key"); err != nil || !replay || item.ScanStatus != "Approved" || item.ScanTaskState != "Succeeded" {
		t.Fatalf("completed retry was not idempotent: %+v %v %v", item, replay, err)
	}
	if _, _, err := s.QueueImageScanRetry(ctx, account, "retry-actor", before.ID, "new-approved-key"); !errors.Is(err, server.ErrImageScanNotRetryable) {
		t.Fatalf("approved image was rescanned: %v", err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE account_id=$1 AND kind='IMAGE_INSPECT'`, account).Scan(&state); err != nil || state != "Failed" {
		t.Fatalf("retry overwrote original failure history: %s %v", state, err)
	}
}

func TestImageScanRetryRejectsForeignDisabledAndNonFailedImages(t *testing.T) {
	s, ctx, account := imageRetryIntegrationStore(t)
	item := imageRetryFixture(t, s, ctx, account)
	foreign, _ := server.NewID()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
		VALUES($1,$2,$2,'Other retry account')`, foreign, "retry-foreign-"+foreign); err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, foreign)
	if _, _, err := s.QueueImageScanRetry(ctx, foreign, "other", item.ID, "foreign-retry-key"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("foreign account could retry image: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE accounts SET enabled=false WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", item.ID, "disabled-retry-key"); !errors.Is(err, server.ErrDisabled) {
		t.Fatalf("disabled owner could retry: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE accounts SET enabled=true WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	for _, taskState := range []string{"Queued", "Leased", "Succeeded"} {
		t.Run(taskState, func(t *testing.T) {
			if _, err := s.Pool.Exec(ctx, `UPDATE image_scan_tasks SET state=$2,
				locked_by=CASE WHEN $2='Leased' THEN 'active-worker' ELSE NULL END,
				lease_until=CASE WHEN $2='Leased' THEN now()+interval '1 minute' ELSE NULL END
				WHERE artifact_id=$1`, item.ID, taskState); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", item.ID, "state-retry-"+taskState); !errors.Is(err, server.ErrImageScanNotRetryable) {
				t.Fatalf("nonfailed task admitted: %v", err)
			}
		})
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE image_scan_tasks SET state='Failed',locked_by=NULL,lease_until=NULL WHERE artifact_id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestImageDeletion(ctx, account, "owner", item.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", item.ID, "gc-retry-key"); !errors.Is(err, server.ErrImageCleanupInProgress) {
		t.Fatalf("GC image could retry: %v", err)
	}
	for _, status := range []string{"PendingScan", "Approved", "Rejected"} {
		t.Run(status, func(t *testing.T) {
			other := imageRetryFixture(t, s, ctx, account)
			if _, err := s.Pool.Exec(ctx, `UPDATE image_artifacts SET scan_status='PendingScan' WHERE id=$1`, other.ID); err != nil {
				t.Fatal(err)
			}
			if status != "PendingScan" {
				evidence := retryScanEvidence(other.PlatformDigest)
				if status == "Rejected" {
					evidence.Total = 1
					evidence.Severities = map[string]int{"Critical": 1}
				}
				ref := harbor.RegistryHost + "/pg-u-" + strings.ReplaceAll(account, "-", "") + "/agent@" + other.PlatformDigest
				if _, _, err := s.recordImageScanDirect(ctx, account, other.ID, ref, evidence); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", other.ID, "status-retry-"+status); !errors.Is(err, server.ErrImageScanNotRetryable) {
				t.Fatalf("nonfailed artifact admitted: %v", err)
			}
		})
	}
}

func TestImageScanRetryConcurrentRequestsAreIdempotent(t *testing.T) {
	s, ctx, account := imageRetryIntegrationStore(t)
	item := imageRetryFixture(t, s, ctx, account)
	var wg sync.WaitGroup
	var replay [2]bool
	var failures [2]error
	for i := range replay {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, replay[i], failures[i] = s.QueueImageScanRetry(ctx, account, "owner", item.ID, "concurrent-retry-key")
		}(i)
	}
	wg.Wait()
	if failures[0] != nil || failures[1] != nil || replay[0] == replay[1] {
		t.Fatalf("same-key concurrency: replay=%v errors=%v", replay, failures)
	}
	other := imageRetryFixture(t, s, ctx, account)
	if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", other.ID, "concurrent-retry-key"); !errors.Is(err, server.ErrImageScanRetryKeyReused) {
		t.Fatalf("same key accepted for different artifact: %v", err)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_SCAN_RETRY'`, account).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate retry audit: count=%d err=%v", count, err)
	}
	if _, _, err := s.QueueImageScanRetry(ctx, account, "owner", item.ID, "different-retry-key"); !errors.Is(err, server.ErrImageScanNotRetryable) {
		t.Fatalf("second key queued duplicate scan: %v", err)
	}
	// Separate browser clicks may use different keys, but still must not reset
	// a retry that the first transaction has already admitted.
	next := imageRetryFixture(t, s, ctx, account)
	for i := range failures {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, failures[i] = s.QueueImageScanRetry(ctx, account, "owner", next.ID,
				[]string{"parallel-retry-first", "parallel-retry-second"}[i])
		}(i)
	}
	wg.Wait()
	accepted, rejected := 0, 0
	for _, err := range failures {
		if err == nil {
			accepted++
		} else if errors.Is(err, server.ErrImageScanNotRetryable) {
			rejected++
		} else {
			t.Fatalf("unexpected parallel retry error: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("parallel distinct keys queued %d times, rejected %d", accepted, rejected)
	}
}

func TestImageScanRetryDoesNotBlockScannerAuditForeignKey(t *testing.T) {
	s, ctx, account := imageRetryIntegrationStore(t)
	item := imageRetryFixture(t, s, ctx, account)
	scanner, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Rollback(context.Background())
	var locked string
	if err := scanner.QueryRow(ctx, `SELECT artifact_id::text FROM image_scan_tasks
		WHERE artifact_id=$1 FOR UPDATE`, item.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	// The retry must wait for the scanner's task transaction, while the scanner
	// must still be able to record an audit referencing the same account.
	retryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := s.QueueImageScanRetry(retryCtx, account, "owner", item.ID, "scanner-lock-retry")
		finished <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a
			WHERE a.wait_event_type='Lock' AND $1::integer=ANY(pg_blocking_pids(a.pid))
			AND a.query LIKE '%FROM image_scan_tasks%')`, int(scanner.Conn().PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("retry did not reach the expected task-lock wait")
	}
	opID, _ := server.NewID()
	if _, err := scanner.Exec(retryCtx, `INSERT INTO operations(id,account_id,actor_subject,kind,state)
		VALUES($1,$2,'scanner','IMAGE_SCAN','Failed')`, opID, account); err != nil {
		t.Fatalf("retry account lock blocked the scanner audit foreign key: %v", err)
	}
	if err := scanner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("retry after scanner commit: %v", err)
		}
	case <-retryCtx.Done():
		t.Fatal("retry did not finish after scanner commit")
	}
}
