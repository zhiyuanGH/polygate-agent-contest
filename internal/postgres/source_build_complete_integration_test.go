package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func TestCompleteLeasedSourceBuildIsAtomicAndAccountScoped(t *testing.T) {
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
	account, _ := server.NewID()
	other, _ := server.NewID()
	for _, id := range []string{account, other} {
		if _, err = s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,source_build_enabled)
			VALUES($1,$2,$2,'Build Completion Test',true)`, id, "build-complete-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean := context.Background()
		_, _ = s.Pool.Exec(clean, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{account, other})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_build_tasks WHERE account_id=ANY($1::uuid[])`, []string{account, other})
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_artifacts WHERE account_id=ANY($1::uuid[])`, []string{account, other})
		_, _ = s.Pool.Exec(clean, `DELETE FROM image_scan_tasks WHERE account_id=ANY($1::uuid[])`, []string{account, other})
		_, _ = s.Pool.Exec(clean, `DELETE FROM image_artifacts WHERE account_id=ANY($1::uuid[])`, []string{account, other})
		_, _ = s.Pool.Exec(clean, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{account, other})
	}()
	archive, _ := server.NewID()
	if _, err = s.ReserveSourceBuild(ctx, account, "student", archive, 100); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, err = s.ActivateSourceBuild(ctx, account, archive, digest); err != nil {
		t.Fatal(err)
	}
	var operationState string
	if err = s.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE account_id=$1
		AND kind='BUILD_SOURCE'`, account).Scan(&operationState); err != nil || operationState != "Queued" {
		t.Fatalf("queued operation: %s %v", operationState, err)
	}
	task, ok, err := s.ClaimNextSourceBuild(ctx, "worker-one", 20*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE account_id=$1
		AND kind='BUILD_SOURCE'`, account).Scan(&operationState); err != nil || operationState != "Building" {
		t.Fatalf("building operation: %s %v", operationState, err)
	}
	project, _ := harbor.AccountProjectName(account)
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	pinned := harbor.RegistryHost + "/" + project + "/agent@" + imageDigest
	image := registry.Image{SubmittedReference: pinned, PinnedReference: pinned,
		ManifestDigest: imageDigest, PlatformDigest: imageDigest, RegistryHost: harbor.RegistryHost,
		OS: "linux", Architecture: "amd64"}
	foreign := image
	foreignProject, _ := harbor.AccountProjectName(other)
	foreign.PinnedReference = harbor.RegistryHost + "/" + foreignProject + "/agent@" + imageDigest
	foreign.SubmittedReference = foreign.PinnedReference
	if _, err = s.CompleteLeasedSourceBuild(ctx, task, "worker-one", foreign); err == nil {
		t.Fatal("cross-account built image was accepted")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE source_build_tasks SET lease_until=now()-interval '1 second'
		WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteLeasedSourceBuild(ctx, task, "worker-one", image); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("expired lease: %v", err)
	}
	var count int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM image_artifacts WHERE account_id=$1`, account).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired lease left image artifact: count=%d err=%v", count, err)
	}
	reclaimed, ok, err := s.ClaimNextSourceBuild(ctx, "worker-two", 20*time.Minute)
	if err != nil || !ok || reclaimed.ID != task.ID || reclaimed.Attempt != 2 {
		t.Fatalf("reclaim: %+v %v %v", reclaimed, ok, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE accounts SET source_build_enabled=false WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteLeasedSourceBuild(ctx, reclaimed, "worker-two", image); err == nil {
		t.Fatal("revoked build was completed")
	} else if de := domain.AsDomainError(err); de == nil || de.Code != "BUILD_NOT_ENABLED" {
		t.Fatalf("revoked build error: %v", err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE accounts SET source_build_enabled=true WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompleteLeasedSourceBuild(ctx, reclaimed, "worker-two", image)
	if err != nil || artifact.ID == "" {
		t.Fatalf("complete: %+v %v", artifact, err)
	}
	var state, resultID string
	if err = s.Pool.QueryRow(ctx, `SELECT state,result_image_artifact_id::text FROM source_build_tasks
		WHERE id=$1`, task.ID).Scan(&state, &resultID); err != nil || state != "Succeeded" || resultID != artifact.ID {
		t.Fatalf("build result: %s %s %v", state, resultID, err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM image_scan_tasks WHERE artifact_id=$1`,
		artifact.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("scan task: count=%d err=%v", count, err)
	}
	var operationResult string
	if err = s.Pool.QueryRow(ctx, `SELECT state,detail->>'imageArtifactId' FROM operations
		WHERE account_id=$1 AND kind='BUILD_SOURCE'`, account).Scan(&operationState, &operationResult); err != nil || operationState != "Succeeded" || operationResult != artifact.ID {
		t.Fatalf("completed operation: %s %s %v", operationState, operationResult, err)
	}
}
