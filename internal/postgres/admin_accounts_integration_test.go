package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"polygate/internal/server"
)

func TestAdminAccountStatusRevokesSessionsAndAuditsChanges(t *testing.T) {
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
            VALUES($1,$2,$2,$2,$3)`, row.id, "admin-test-"+row.id, row.role); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM admin_account_audit WHERE actor_account_id=$1`, adminID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM auth_sessions WHERE account_id=$1`, studentID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id IN ($1,$2)`, adminID, studentID)
	}()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO auth_sessions(session_hash,account_id,csrf_hash,expires_at)
        VALUES(decode('aabbcc','hex'),$1,decode('ddeeff','hex'),now()+interval '1 hour')`, studentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListAdminAccounts(ctx, studentID); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("student read: %v", err)
	}
	if _, err := store.SetAdminAccountEnabled(ctx, studentID, "student", adminID, false, "test reason"); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("student write: %v", err)
	}
	if _, err := store.SetAdminAccountEnabled(ctx, adminID, "admin", adminID, false, "test reason"); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("self disable: %v", err)
	}
	accounts, err := store.ListAdminAccounts(ctx, adminID)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("admin read: %d %v", len(accounts), err)
	}
	account, err := store.SetAdminAccountEnabled(ctx, adminID, "admin-subject", studentID, false, "Contest account suspended")
	if err != nil || account.Enabled || account.ID != studentID {
		t.Fatalf("disable: %+v %v", account, err)
	}
	if _, err := store.Account(ctx, studentID); !errors.Is(err, server.ErrDisabled) {
		t.Fatalf("disabled account access: %v", err)
	}
	var sessions int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE account_id=$1`, studentID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("session revocation: %d %v", sessions, err)
	}
	if _, err := store.SetAdminAccountEnabled(ctx, adminID, "admin-subject", studentID, false, "Repeated request"); err != nil {
		t.Fatal(err)
	}
	audit, err := store.ListAdminAccountAudit(ctx, adminID, 100)
	if err != nil || len(audit) != 1 || audit[0].Action != "DISABLE_ACCOUNT" || audit[0].Reason != "Contest account suspended" || audit[0].ActorUsername != "admin-test-"+adminID {
		t.Fatalf("disable audit: %+v %v", audit, err)
	}
	account, err = store.SetAdminAccountEnabled(ctx, adminID, "admin-subject", studentID, true, "Appeal accepted")
	if err != nil || !account.Enabled {
		t.Fatalf("enable: %+v %v", account, err)
	}
	audit, err = store.ListAdminAccountAudit(ctx, adminID, 100)
	if err != nil || len(audit) != 2 || audit[0].Action != "ENABLE_ACCOUNT" {
		t.Fatalf("enable audit: %+v %v", audit, err)
	}
	if _, err := store.SetAdminAccountEnabled(ctx, adminID, "admin-subject", "00000000-0000-4000-8000-000000000001", false, "Missing account"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
}
