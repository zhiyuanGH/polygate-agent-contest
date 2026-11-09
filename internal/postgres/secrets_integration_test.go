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

func TestSecretCreationAndDeletionSerializeOnAccount(t *testing.T) {
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
	accountID, err := server.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
		VALUES($1,$2,$2,$2,'student')`, accountID, "secret-test-"+accountID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	}()
	if err := store.PutSecretWithAudit(ctx, accountID, "student-subject", "MODEL_API_KEY",
		func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := store.Pool.QueryRow(ctx, `SELECT detail::text FROM operations WHERE account_id=$1 AND kind='SECRET_UPSERT'`,
		accountID).Scan(&detail); err != nil || !strings.Contains(detail, "MODEL_API_KEY") || strings.Contains(detail, "secret-value") {
		t.Fatalf("audit detail: %q %v", detail, err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	created := make(chan error, 1)
	spec := domain.AppSpec{Name: "with-secret", Image: "example.com/agent:v1", Port: 8080,
		Resources:  domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1},
		SecretKeys: []string{"MODEL_API_KEY"}}
	go func() {
		_, _, err := store.CreateWithSecretCheck(ctx, accountID, "student-subject", "secret-app-create", spec,
			func(context.Context) error { close(entered); <-release; return nil })
		created <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	deleted := make(chan error, 1)
	called := make(chan struct{}, 1)
	go func() {
		deleted <- store.DeleteSecretIfUnused(ctx, accountID, "student-subject", "MODEL_API_KEY",
			func(context.Context, string) error { called <- struct{}{}; return nil })
	}()
	close(release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; !errors.Is(err, server.ErrSecretInUse) {
		t.Fatalf("concurrent deletion: %v", err)
	}
	select {
	case <-called:
		t.Fatal("referenced key was deleted")
	default:
	}
}

func TestSecretMutationIntentRecoversAfterCanceledDatabaseCommit(t *testing.T) {
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
	accountID, err := server.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
		VALUES($1,$2,$2,$2,'student')`, accountID, "secret-recovery-"+accountID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	}()
	requestCtx, interrupt := context.WithCancel(ctx)
	var appliedID string
	err = store.PutSecretWithAudit(requestCtx, accountID, "student-subject", "MODEL_API_KEY",
		func(_ context.Context, operationID string) error {
			appliedID = operationID // the Kubernetes update has returned successfully
			interrupt()             // database finalize now loses its request context
			return nil
		})
	if err == nil || appliedID == "" {
		t.Fatalf("expected an uncertain database finalize; operation=%q err=%v", appliedID, err)
	}
	var state string
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, appliedID).Scan(&state); err != nil || state != "Pending" {
		t.Fatalf("durable intent: state=%q err=%v", state, err)
	}
	if err := store.PutSecretWithAudit(ctx, accountID, "student-subject", "OTHER_TOKEN",
		func(context.Context, string) error { t.Fatal("new mutation bypassed pending intent"); return nil }); !errors.Is(err, ErrSecretMutationPending) {
		t.Fatalf("pending operation did not block next mutation: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE operations SET created_at=now()-interval '1 minute' WHERE id=$1`, appliedID); err != nil {
		t.Fatal(err)
	}
	if healthy, err := store.SecretMutationBacklogHealthy(ctx); err != nil || !healthy {
		t.Fatalf("fresh pending intent triggered backlog: healthy=%v err=%v", healthy, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE operations SET created_at=now()-interval '3 minutes' WHERE id=$1`, appliedID); err != nil {
		t.Fatal(err)
	}
	if healthy, err := store.SecretMutationBacklogHealthy(ctx); err != nil || healthy {
		t.Fatalf("stale pending intent was missed: healthy=%v err=%v", healthy, err)
	}
	resolved, err := store.ReconcileSecretMutations(ctx,
		func(_ context.Context, account, key, kind, operationID string) (bool, error) {
			return account == accountID && key == "MODEL_API_KEY" && kind == "SECRET_UPSERT" && operationID == appliedID, nil
		})
	if err != nil || resolved != 1 {
		t.Fatalf("reconcile applied operation: resolved=%d err=%v", resolved, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, appliedID).Scan(&state); err != nil || state != "Succeeded" {
		t.Fatalf("reconciled audit: state=%q err=%v", state, err)
	}
	if healthy, err := store.SecretMutationBacklogHealthy(ctx); err != nil || !healthy {
		t.Fatalf("recovered intent still triggered backlog: healthy=%v err=%v", healthy, err)
	}
	if err := store.PutSecretWithAudit(ctx, accountID, "student-subject", "OTHER_TOKEN",
		func(context.Context, string) error { return nil }); err != nil {
		t.Fatalf("mutation after recovery: %v", err)
	}
	var failedID string
	if err := store.PutSecretWithAudit(ctx, accountID, "student-subject", "UNAPPLIED_TOKEN",
		func(_ context.Context, operationID string) error {
			failedID = operationID
			return errors.New("simulated Kubernetes failure")
		}); err == nil || failedID == "" {
		t.Fatalf("expected failed external update: operation=%q err=%v", failedID, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE operations SET created_at=now()-interval '1 minute' WHERE id=$1`, failedID); err != nil {
		t.Fatal(err)
	}
	if resolved, err := store.ReconcileSecretMutations(ctx,
		func(context.Context, string, string, string, string) (bool, error) {
			return false, errors.New("Kubernetes read temporarily unavailable")
		}); err == nil || resolved != 0 {
		t.Fatalf("read failure must leave intent unresolved: resolved=%d err=%v", resolved, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, failedID).Scan(&state); err != nil || state != "Pending" {
		t.Fatalf("read failure changed audit: state=%q err=%v", state, err)
	}
	if resolved, err := store.ReconcileSecretMutations(ctx,
		func(_ context.Context, account, key, kind, operationID string) (bool, error) {
			if account != accountID || key != "UNAPPLIED_TOKEN" || kind != "SECRET_UPSERT" || operationID != failedID {
				t.Fatal("reconciler inspected the wrong intent")
			}
			return false, nil
		}); err != nil || resolved != 1 {
		t.Fatalf("unapplied intent did not fail: resolved=%d err=%v", resolved, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, failedID).Scan(&state); err != nil || state != "Failed" {
		t.Fatalf("unapplied audit: state=%q err=%v", state, err)
	}
}
