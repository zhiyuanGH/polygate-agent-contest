package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"polygate/internal/server"
)

func TestSourceBuildRevocationTerminatesQueuedLeasedAndPreparing(t *testing.T) {
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
	if _, err = s.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,source_build_enabled)
		VALUES($1,$2,$2,'Build Revocation Test',true)`, account, "build-revoke-"+account); err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean := context.Background()
		_, _ = s.Pool.Exec(clean, `DELETE FROM operations WHERE account_id=$1`, account)
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_build_tasks WHERE account_id=$1`, account)
		_, _ = s.Pool.Exec(clean, `DELETE FROM source_artifacts WHERE account_id=$1`, account)
		_, _ = s.Pool.Exec(clean, `DELETE FROM accounts WHERE id=$1`, account)
	}()
	digest := "sha256:" + strings.Repeat("a", 64)
	queue := func(activate bool) {
		id, _ := server.NewID()
		if _, err := s.ReserveSourceBuild(ctx, account, "student", id, 100); err != nil {
			t.Fatal(err)
		}
		if activate {
			if _, err := s.ActivateSourceBuild(ctx, account, id, digest); err != nil {
				t.Fatal(err)
			}
		}
	}
	queue(true)
	if _, ok, err := s.ClaimNextSourceBuild(ctx, "revocation-worker", 20*time.Minute); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	queue(true)
	queue(false)
	if _, err = s.Pool.Exec(ctx, `UPDATE accounts SET source_build_enabled=false WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	count, err := s.ReconcileRevokedSourceBuilds(ctx)
	if err != nil || count != 3 {
		t.Fatalf("reconcile: %d %v", count, err)
	}
	count, err = s.ReconcileRevokedSourceBuilds(ctx)
	if err != nil || count != 0 {
		t.Fatalf("idempotent reconcile: %d %v", count, err)
	}
	var tasks, operations int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM source_build_tasks WHERE account_id=$1
		AND state='Failed' AND error_code='BUILD_NOT_ENABLED' AND locked_by IS NULL`, account).Scan(&tasks); err != nil || tasks != 3 {
		t.Fatalf("failed tasks: %d %v", tasks, err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE account_id=$1
		AND kind='BUILD_SOURCE' AND state='Failed' AND detail->>'errorCode'='BUILD_NOT_ENABLED'`, account).Scan(&operations); err != nil || operations != 3 {
		t.Fatalf("failed audit operations: %d %v", operations, err)
	}
}
