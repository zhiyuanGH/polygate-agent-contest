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

func TestAdminQuotaChangeCommitsOnlyAfterClusterApplyAndRetries(t *testing.T) {
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
	adminID, _ := server.NewID()
	studentID, _ := server.NewID()
	for _, row := range []struct{ id, role string }{{adminID, "admin"}, {studentID, "student"}} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
            VALUES($1,$2,$2,$2,$3)`, row.id, "quota-test-"+row.id, row.role); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM account_quota_changes WHERE target_account_id=$1`, studentID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=$1`, studentID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=$1`, studentID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id IN ($1,$2)`, adminID, studentID)
	}()
	desired := domain.Quota{CPURequestMilli: 3000, CPULimitMilli: 6000, MemoryMiB: 8192, Pods: 5, Apps: 3}
	spec := domain.AppSpec{Name: "quota-pending-draft", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	draft, _, err := store.Create(ctx, studentID, "student", "quota-draft-before", spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Delete(context.Background(), studentID, "student", draft.ID) }()
	if _, _, err := store.QueueAdminQuotaChange(ctx, studentID, "student", adminID,
		"student-key-001", "not allowed", desired); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("student queue: %v", err)
	}
	change, replay, err := store.QueueAdminQuotaChange(ctx, adminID, "admin-subject", studentID,
		"quota-key-001", "Contest accommodation", desired)
	if err != nil || replay || change.State != "Queued" {
		t.Fatalf("queue: %+v %v %v", change, replay, err)
	}
	spec.Name = "quota-pending-blocked"
	if _, _, err := store.Create(ctx, studentID, "student", "quota-draft-blocked", spec); domain.AsDomainError(err) == nil || domain.AsDomainError(err).Code != "QUOTA_CHANGE_PENDING" {
		t.Fatalf("new draft admitted during quota change: %v", err)
	}
	if err := store.Delete(ctx, studentID, "student", draft.ID); err != nil {
		t.Fatalf("draft deletion blocked during quota change: %v", err)
	}
	duplicate, replay, err := store.QueueAdminQuotaChange(ctx, adminID, "admin-subject", studentID,
		"quota-key-001", "Contest accommodation", desired)
	if err != nil || !replay || duplicate.ID != change.ID {
		t.Fatalf("idempotency: %+v %v %v", duplicate, replay, err)
	}
	if _, _, err := store.QueueAdminQuotaChange(ctx, adminID, "admin-subject", studentID,
		"quota-key-002", "Second request", desired); domain.AsDomainError(err) == nil {
		t.Fatalf("parallel change accepted: %v", err)
	}
	fail := errors.New("simulated Kubernetes outage")
	processed, err := store.ProcessNextQuotaChange(ctx, func(_ context.Context, id string, q domain.Quota) error {
		if id != studentID || q != desired {
			t.Fatalf("unexpected apply: %s %+v", id, q)
		}
		return fail
	})
	if !processed || !errors.Is(err, fail) {
		t.Fatalf("failure: %v %v", processed, err)
	}
	account, err := store.Account(ctx, studentID)
	if err != nil || account.Quota != domain.StudentQuota {
		t.Fatalf("quota committed before cluster: %+v %v", account.Quota, err)
	}
	if _, _, err := store.Create(ctx, studentID, "student", "quota-draft-after-failure", spec); domain.AsDomainError(err) == nil || domain.AsDomainError(err).Code != "QUOTA_CHANGE_PENDING" {
		t.Fatalf("new draft admitted after failed apply: %v", err)
	}
	changes, err := store.ListAdminQuotaChanges(ctx, adminID)
	if err != nil || len(changes) != 1 || changes[0].State != "Queued" || changes[0].LastError == "" {
		t.Fatalf("failure record: %+v %v", changes, err)
	}
	processed, err = store.ProcessNextQuotaChange(ctx, func(_ context.Context, id string, q domain.Quota) error {
		if id != studentID || q != desired {
			t.Fatalf("retry changed task: %s %+v", id, q)
		}
		return nil
	})
	if !processed || err != nil {
		t.Fatalf("retry: %v %v", processed, err)
	}
	account, err = store.Account(ctx, studentID)
	if err != nil || account.Quota != desired {
		t.Fatalf("quota not committed: %+v %v", account.Quota, err)
	}
	newDraft, _, err := store.Create(ctx, studentID, "student", "quota-draft-after-apply", spec)
	if err != nil {
		t.Fatalf("new draft blocked after quota applied: %v", err)
	}
	if err := store.Delete(ctx, studentID, "student", newDraft.ID); err != nil {
		t.Fatal(err)
	}
	changes, err = store.ListAdminQuotaChanges(ctx, adminID)
	if err != nil || len(changes) != 1 || changes[0].State != "Applied" || changes[0].AppliedAt == nil {
		t.Fatalf("applied record: %+v %v", changes, err)
	}
	if processed, err := store.ProcessNextQuotaChange(ctx, func(context.Context, string, domain.Quota) error {
		t.Fatal("no task should remain")
		return nil
	}); err != nil || processed {
		t.Fatalf("empty queue: %v %v", processed, err)
	}
}
