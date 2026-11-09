package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestSourceBuildQueueLeasesAndOwnership(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := server.NewID()
	b, _ := server.NewID()
	for _, id := range []string{a, b} {
		_, err = s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,source_build_enabled)
			VALUES($1,$2,$2,'Build Test',true)`, id, "build-"+id)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean := context.Background()
		_, _ = s.Pool.Exec(clean, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_build_tasks WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_artifacts WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM image_scan_tasks WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM image_artifacts WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{a, b})
	}()
	digest := "sha256:" + strings.Repeat("a", 64)
	queue := func(account string) SourceBuildTask {
		id, _ := server.NewID()
		_, err := s.ReserveSourceBuild(ctx, account, "build-test", id, 321)
		if err != nil {
			t.Fatal(err)
		}
		task, err := s.ActivateSourceBuild(ctx, account, id, digest)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := queue(a)
	queue(a)
	queue(a)
	id, _ := server.NewID()
	_, err = s.ReserveSourceBuild(ctx, a, "build-test", id, 321)
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) || domainErr.Code != "BUILD_QUEUE_FULL" {
		t.Fatalf("queue limit: %v", err)
	}
	bTask := queue(b)
	items, err := s.ListSourceBuilds(ctx, b)
	if err != nil || len(items) != 1 || items[0].ID != bTask.ID {
		t.Fatalf("account listing: %+v %v", items, err)
	}
	claimed, ok, err := s.ClaimNextSourceBuild(ctx, "worker-one", 20*time.Minute)
	if err != nil || !ok || claimed.ID != first.ID || claimed.Attempt != 1 {
		t.Fatalf("first claim: %+v %v %v", claimed, ok, err)
	}
	healthy, err := s.SourceQueueHealthy(ctx)
	if err != nil || !healthy {
		t.Fatalf("live source lease must be healthy: %v %v", healthy, err)
	}
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET lease_until=now()-interval '3 minutes' WHERE id=$1`, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err = s.SourceQueueHealthy(ctx)
	if err != nil || healthy {
		t.Fatalf("stale source lease must be unhealthy: %v %v", healthy, err)
	}
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET lease_until=now()+interval '20 minutes' WHERE id=$1`, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The other global slot must go to B while A has a live lease.
	other, ok, err := s.ClaimNextSourceBuild(ctx, "worker-two", 20*time.Minute)
	if err != nil || !ok || other.ID != bTask.ID {
		t.Fatalf("second claim: %+v %v %v", other, ok, err)
	}
	_, ok, err = s.ClaimNextSourceBuild(ctx, "worker-three", 20*time.Minute)
	if err != nil || ok {
		t.Fatalf("global limit: %v %v", ok, err)
	}
	if err = s.FailSourceBuild(ctx, first.ID, "worker-two", 1, "BUILD_FAILED", false); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("foreign lease: %v", err)
	}
	if err = s.FailSourceBuild(ctx, first.ID, "worker-one", 1, "BUILD_FAILED", true); err != nil {
		t.Fatal(err)
	}
	// A new claim can proceed after the first task releases its slot.
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET next_attempt_at=now()+interval '1 hour' WHERE id=$1`, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	third, ok, err := s.ClaimNextSourceBuild(ctx, "worker-three", 20*time.Minute)
	if err != nil || !ok || third.AccountID != a || third.ID == first.ID {
		t.Fatalf("freed slot: %+v %v %v", third, ok, err)
	}
	// Expiry lets a new worker reclaim the task, but fences the old attempt.
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET lease_until=now()-interval '1 second' WHERE id=$1`, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := s.ClaimNextSourceBuild(ctx, "worker-four", 20*time.Minute)
	if err != nil || !ok || reclaimed.ID != third.ID || reclaimed.Attempt != 2 {
		t.Fatalf("lease recovery: %+v %v %v", reclaimed, ok, err)
	}
	if err = s.FailSourceBuild(ctx, third.ID, "worker-three", 1, "BUILD_FAILED", false); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("stale attempt: %v", err)
	}
	if err = s.FailSourceBuild(ctx, third.ID, "worker-four", 2, "BUILD_FAILED", false); err != nil {
		t.Fatal(err)
	}
	if err = s.FailSourceBuild(ctx, bTask.ID, "worker-two", 1, "BUILD_FAILED", false); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBuildReservationExpiryAndAccountQuota(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := server.NewID()
	b, _ := server.NewID()
	for _, id := range []string{a, b} {
		_, err = s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,source_build_enabled)
			VALUES($1,$2,$2,'Build Quota Test',true)`, id, "quota-"+id)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean := context.Background()
		_, _ = s.Pool.Exec(clean, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_build_tasks WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_artifacts WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = s.Pool.Exec(clean, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{a, b})
	}()
	id, _ := server.NewID()
	prepared, err := s.ReserveSourceBuild(ctx, a, "student", id, 100)
	if err != nil || prepared.State != "Preparing" {
		t.Fatalf("reserve: %+v %v", prepared, err)
	}
	_, ok, err := s.ClaimNextSourceBuild(ctx, "worker", 20*time.Minute)
	if err != nil || ok {
		t.Fatalf("claimed unfinished ZIP: %v %v", ok, err)
	}
	if _, err = s.ActivateSourceBuild(ctx, b, id, "sha256:"+strings.Repeat("a", 64)); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account activate: %v", err)
	}
	ready, err := s.ActivateSourceBuild(ctx, a, id, "sha256:"+strings.Repeat("a", 64))
	if err != nil || ready.State != "Queued" {
		t.Fatalf("activate: %+v %v", ready, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE accounts SET source_build_enabled=false WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	revokedArtifact, _ := server.NewID()
	if _, err = s.ReserveSourceBuild(ctx, a, "student", revokedArtifact, 100); err == nil {
		t.Fatal("revoked account reserved a build")
	}
	if _, ok, err = s.ClaimNextSourceBuild(ctx, "worker", 20*time.Minute); err != nil || ok {
		t.Fatalf("revoked account build was claimed: %v %v", ok, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE accounts SET source_build_enabled=true WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextSourceBuild(ctx, "worker", 20*time.Minute)
	if err != nil || !ok || claimed.ID != ready.ID {
		t.Fatalf("claim ready: %+v %v %v", claimed, ok, err)
	}
	if err = s.FailSourceBuild(ctx, ready.ID, "worker", 1, "BUILD_FAILED", false); err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET updated_at=now()-interval '15 days' WHERE id=$1`, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := s.ExpireSourceBuilds(ctx)
	if err != nil || len(expired) != 1 || expired[0].ArtifactID != id || expired[0].TaskID != ready.ID {
		t.Fatalf("retention: %v %v", expired, err)
	}
	for i := 0; i < 10; i++ {
		archiveID, _ := server.NewID()
		_, err = s.Pool.Exec(ctx, `INSERT INTO source_artifacts(id,account_id,size_bytes) VALUES($1,$2,$3)`, archiveID, a, 100<<20)
		if err != nil {
			t.Fatal(err)
		}
	}
	extra, _ := server.NewID()
	_, err = s.ReserveSourceBuild(ctx, a, "student", extra, 30<<20)
	var quotaErr *domain.Error
	if !errors.As(err, &quotaErr) || quotaErr.Code != "SOURCE_STORAGE_FULL" {
		t.Fatalf("storage quota: %v", err)
	}
	stale, _ := server.NewID()
	staleTask, err := s.ReserveSourceBuild(ctx, b, "student", stale, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET created_at=now()-interval '25 hours' WHERE artifact_id=$1`, stale)
	if err != nil {
		t.Fatal(err)
	}
	preparing, err := s.ListPreparingSourceBuilds(ctx, 5*time.Minute)
	if err != nil || len(preparing) != 1 || preparing[0].ArtifactID != stale {
		t.Fatalf("recovery list: %+v %v", preparing, err)
	}
	expired, err = s.ExpireSourceBuilds(ctx)
	if err != nil || len(expired) != 1 || expired[0].ArtifactID != stale || expired[0].TaskID != staleTask.ID {
		t.Fatalf("abandoned reservation: %v %v", expired, err)
	}
	var expiredOperationState, expiredCode string
	err = s.Pool.QueryRow(ctx, `SELECT state,detail->>'errorCode' FROM operations
		WHERE account_id=$1 AND kind='BUILD_SOURCE' AND detail->>'taskId'=$2`,
		b, staleTask.ID).Scan(&expiredOperationState, &expiredCode)
	if err != nil || expiredOperationState != "Failed" || expiredCode != "SOURCE_UPLOAD_EXPIRED" {
		t.Fatalf("expired upload operation: %s %s %v", expiredOperationState, expiredCode, err)
	}
	failedUpload, _ := server.NewID()
	_, err = s.ReserveSourceBuild(ctx, b, "student", failedUpload, 100)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.AbandonSourceBuild(ctx, b, failedUpload)
	if err != nil || !removed {
		t.Fatalf("abandon: %v %v", removed, err)
	}
	referenced, err := s.SourceArtifactReferenced(ctx, failedUpload)
	if err != nil || referenced {
		t.Fatalf("quota reservation persisted: %v %v", referenced, err)
	}
	removed, err = s.AbandonSourceBuild(ctx, b, failedUpload)
	if err != nil || removed {
		t.Fatalf("duplicate abandon: %v %v", removed, err)
	}
}
