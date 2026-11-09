package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestDraftResourceEditIsAccountScopedAndAtomic(t *testing.T) {
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
	owner, _ := server.NewID()
	other, _ := server.NewID()
	for _, id := range []string{owner, other} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
			VALUES($1,$2,$2,'Resource edit pilot')`, id, "resources-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{owner, other})
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, []string{owner, other})
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{owner, other})
	}()
	spec := domain.AppSpec{Name: "resource-app", Image: "example.com/app:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, owner, "owner", "resource-create-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	changed := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 2}
	if _, err = store.UpdateAppResources(ctx, other, "other", app.ID, changed); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account resource edit: %v", err)
	}
	updated, err := store.UpdateAppResources(ctx, owner, "owner", app.ID, changed)
	if err != nil || updated.Spec.Resources != changed {
		t.Fatalf("owner resource edit: %+v %v", updated, err)
	}
	_, usage, err := store.List(ctx, owner)
	if err != nil || usage.MemoryMiB != 2048 || usage.Pods != 2 {
		t.Fatalf("configured usage: %+v %v", usage, err)
	}
	tooLarge := changed
	tooLarge.MemoryMiB = 4096
	if _, err = store.UpdateAppResources(ctx, owner, "owner", app.ID, tooLarge); domain.AsDomainError(err) == nil {
		t.Fatalf("over-quota resource edit: %v", err)
	}
	stored, err := store.Get(ctx, owner, app.ID)
	if err != nil || stored.Spec.Resources != changed {
		t.Fatalf("rejected edit changed app: %+v %v", stored, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE apps SET status='Running' WHERE id=$1`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpdateAppResources(ctx, owner, "owner", app.ID, spec.Resources); domain.AsDomainError(err) == nil {
		t.Fatalf("running edit was accepted: %v", err)
	}
	operations, err := store.ListOperations(ctx, owner)
	if err != nil || len(operations) != 2 || operations[0].Kind != "UPDATE_APP_RESOURCES" {
		t.Fatalf("resource edit audit: %+v %v", operations, err)
	}
}
