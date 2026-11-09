package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func TestImageArtifactOwnershipAndRevisionConstraint(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	aID, _ := server.NewID()
	bID, _ := server.NewID()
	for _, id := range []string{aID, bID} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
			VALUES($1,$2,$2,'Image Test')`, id, "image-test-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM app_revisions WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM image_artifacts WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{aID, bID})
	}()
	digest := "sha256:" + strings.Repeat("a", 64)
	image := registry.Image{OS: "linux", Architecture: "amd64", RegistryHost: "registry.example",
		SubmittedReference: "registry.example/team/agent:v1",
		PinnedReference:    "registry.example/team/agent@" + digest,
		ManifestDigest:     digest, PlatformDigest: "sha256:" + strings.Repeat("b", 64)}
	first, replay, err := store.SaveImageArtifact(ctx, aID, "subject-a", image)
	if err != nil || replay || first.ScanStatus != "PendingScan" {
		t.Fatalf("first artifact: %+v, replay=%v, err=%v", first, replay, err)
	}
	second, replay, err := store.SaveImageArtifact(ctx, aID, "subject-a", image)
	if err != nil || !replay || second.ID != first.ID {
		t.Fatalf("idempotent artifact: %+v, replay=%v, err=%v", second, replay, err)
	}
	for _, tc := range []struct {
		account string
		want    int
	}{{aID, 1}, {bID, 0}} {
		items, err := store.ListImageArtifacts(ctx, tc.account)
		if err != nil || len(items) != tc.want {
			t.Fatalf("list account %s: %d, %v", tc.account, len(items), err)
		}
	}
	var auditCount int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_INSPECT'`, aID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("inspect audit count %d, %v", auditCount, err)
	}
	spec := domain.AppSpec{Name: "cross-owner", Image: image.PinnedReference, Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, bID, "subject-b", "cross-owner-create", spec)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := server.NewID()
	_, err = store.Pool.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec,source_kind,image_artifact_id)
		VALUES($1,$2,$3,1,$4,'{}','image',$5)`, revisionID, bID, app.ID, image.PinnedReference, first.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("cross-account artifact FK should reject revision, got %v", err)
	}
	_, err = store.Pool.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec,source_kind)
		VALUES($1,$2,$3,1,$4,'{}','image')`, revisionID, bID, app.ID, image.PinnedReference)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("image revision without artifact should fail CHECK, got %v", err)
	}
	for i := 0; i < maxImageArtifactsPerAccount-1; i++ {
		another := image
		another.ManifestDigest = fmt.Sprintf("sha256:%064x", i+1)
		another.PinnedReference = "registry.example/team/agent@" + another.ManifestDigest
		if _, _, err := store.SaveImageArtifact(ctx, aID, "subject-a", another); err != nil {
			t.Fatalf("artifact %d: %v", i, err)
		}
	}
	overLimit := image
	overLimit.ManifestDigest = fmt.Sprintf("sha256:%064x", maxImageArtifactsPerAccount+1)
	overLimit.PinnedReference = "registry.example/team/agent@" + overLimit.ManifestDigest
	if _, _, err := store.SaveImageArtifact(ctx, aID, "subject-a", overLimit); domain.AsDomainError(err) == nil ||
		domain.AsDomainError(err).Code != "IMAGE_ARTIFACT_LIMIT" {
		t.Fatalf("artifact limit: %v", err)
	}
	if duplicate, replay, err := store.SaveImageArtifact(ctx, aID, "subject-a", image); err != nil ||
		!replay || duplicate.ID != first.ID {
		t.Fatalf("existing digest should replay at the limit: %+v, %v, %v", duplicate, replay, err)
	}
}
