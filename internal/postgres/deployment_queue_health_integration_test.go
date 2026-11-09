package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"polygate/internal/server"
)

func TestDeploymentQueueHealthDetectsStaleWorkAndLease(t *testing.T) {
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
	accountID, _ := server.NewID()
	appID, _ := server.NewID()
	revisionID, _ := server.NewID()
	taskID, _ := server.NewID()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
		VALUES($1,$2,$2,$2)`, accountID, "queue-health-"+accountID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM deployment_tasks WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM app_revisions WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	}()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO apps(id,account_id,name,image,port,
		cpu_request_milli,cpu_limit_milli,memory_mib,replicas,idempotency_key)
		VALUES($1,$2,'queue-test','example.com/agent:v1',8080,250,1000,512,1,'queue-app')`, appID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec)
		VALUES($1,$2,$3,1,'example.com/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','{}'::jsonb)`,
		revisionID, accountID, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,
		idempotency_key,desired_state) VALUES($1,$2,$3,$4,'queue-task','Running')`,
		taskID, accountID, appID, revisionID); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		healthy, err := store.DeploymentQueueHealthy(ctx)
		if err != nil || healthy != want {
			t.Fatalf("deployment queue healthy=%v, want %v, err=%v", healthy, want, err)
		}
	}
	check(true)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET created_at=now()-interval '31 minutes'
		WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET created_at=now(),state='Leased',
		lease_until=now()-interval '3 minutes',locked_by='stale-worker' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET lease_until=NULL WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET state='Succeeded',lease_until=NULL,
		locked_by=NULL WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	check(true)
}
