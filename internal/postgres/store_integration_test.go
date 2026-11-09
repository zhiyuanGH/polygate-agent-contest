package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestPostgresAccountIsolationAndDurability(t *testing.T) {
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
	defer func() { store.Close() }()
	aID, _ := server.NewID()
	bID, _ := server.NewID()
	for _, id := range []string{aID, bID} {
		_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
            VALUES($1,$2,$2,'Integration Student')`, id, "test-"+id)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{aID, bID})
	}()
	spec := domain.AppSpec{Name: "agent", Image: "example.com/agent:v1", Port: 8080, HealthPath: "/health",
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	a, _, err := store.Create(ctx, aID, "sub-a", "first-account-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Create(ctx, bID, "sub-b", "second-account-key", spec); err != nil {
		t.Fatal("same app name across accounts:", err)
	}
	if _, err = store.Get(ctx, bID, a.ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account read = %v", err)
	}
	if err = store.Delete(ctx, bID, "sub-b", a.ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account delete = %v", err)
	}
	digest := "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, _, err = store.QueueTrustedDeploy(ctx, bID, "sub-b", a.ID, "cross-account-deploy", digest, 5678); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account deploy = %v", err)
	}
	if _, _, err = store.QueueStop(ctx, bID, "sub-b", a.ID, "cross-account-stop"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account stop = %v", err)
	}
	if replay, replayed, err := store.Create(ctx, aID, "sub-a", "first-account-key", spec); err != nil || !replayed || replay.ID != a.ID {
		t.Fatalf("idempotent replay: %v %v %v", replay, replayed, err)
	}
	if err = store.RecordSourceValidation(ctx, aID, "sub-a", 456, "Succeeded", ""); err != nil {
		t.Fatal("record source validation:", err)
	}
	aOps, err := store.ListOperations(ctx, aID)
	if err != nil || len(aOps) != 2 {
		t.Fatalf("account A operations: %d %v", len(aOps), err)
	}
	bOps, err := store.ListOperations(ctx, bID)
	if err != nil || len(bOps) != 1 {
		t.Fatalf("account B operations: %d %v", len(bOps), err)
	}
	for _, op := range bOps {
		if op.AppID == a.ID || op.Kind == "VALIDATE_ZIP" {
			t.Fatalf("other account operation visible: %+v", op)
		}
	}
	store.Close()
	store, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, aID, a.ID)
	if err != nil {
		t.Fatal("draft lost after reconnect:", err)
	}
	if got.Spec.HealthPath != spec.HealthPath {
		t.Fatalf("health path lost after reconnect: %q", got.Spec.HealthPath)
	}
	if aOps, err = store.ListOperations(ctx, aID); err != nil || len(aOps) != 2 {
		t.Fatalf("operations lost after reconnect: %d %v", len(aOps), err)
	}
	var wg sync.WaitGroup
	accepted := make(chan bool, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			item := spec
			item.Name = fmt.Sprintf("parallel-%d", i)
			_, _, err := store.Create(ctx, aID, "sub-a", fmt.Sprintf("parallel-key-%d", i), item)
			accepted <- err == nil
		}(i)
	}
	wg.Wait()
	close(accepted)
	n := 0
	for ok := range accepted {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("accepted %d additional drafts, want 1", n)
	}
	apps, usage, err := store.List(ctx, aID)
	if err != nil || len(apps) != 2 || usage.Apps != 2 {
		t.Fatalf("account usage: %v %+v %v", apps, usage, err)
	}
	other, _, err := store.List(ctx, bID)
	if err != nil || len(other) != 1 {
		t.Fatalf("other account list: %v %v", other, err)
	}
	if err = store.Delete(ctx, aID, "sub-a", a.ID); err != nil {
		t.Fatalf("owner could not delete draft: %v", err)
	}
}
