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

func TestAdminAppAccessSwitchRevokesSessionsAndKeepsAudit(t *testing.T) {
	databaseURL := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	adminID, _ := server.NewID()
	studentID, _ := server.NewID()
	ids := []string{adminID, studentID}
	for _, row := range []struct{ id, role string }{{adminID, "admin"}, {studentID, "student"}} {
		if _, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
			VALUES($1,$2,$2,$2,$3)`, row.id, "app-mode-"+row.id, row.role); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM admin_app_access_audit WHERE actor_account_id=$1`, adminID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, ids)
	}()
	spec := domain.AppSpec{Name: "access-admin", Image: "example.com/app:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	app, _, err := store.Create(ctx, studentID, "student", "access-admin", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetAdminAppAccess(ctx, studentID, "student", app.ID, AppAccessPrivate, "Contest review"); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("student changed access = %v", err)
	}
	if _, err = store.SetAdminAppAccess(ctx, adminID, "admin", app.ID, AppAccessPrivate, "Contest review"); domain.AsDomainError(err) == nil {
		t.Fatalf("draft restricted without route = %v", err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE apps SET status='Running' WHERE id=$1`, app.ID); err != nil {
		t.Fatal(err)
	}
	changed, err := store.SetAdminAppAccess(ctx, adminID, "admin", app.ID, AppAccessPrivate, "Contest review")
	if err != nil || changed.AccessMode != AppAccessPrivate || changed.Username != "app-mode-"+studentID {
		t.Fatalf("switch to private = %+v, %v", changed, err)
	}
	if allowed, err := store.PublicAppAllowed(ctx, app.ID); err != nil || allowed {
		t.Fatalf("public path after switch = %v, %v", allowed, err)
	}
	ticket, err := store.IssueAppAccessTicket(ctx, studentID, app.ID, "/")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.ConsumeAppAccessTicket(ctx, app.ID, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetAdminAppAccess(ctx, adminID, "admin", app.ID, AppAccessPublic, "Contest review complete"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, session); err != nil || allowed {
		t.Fatalf("old session survived mode change = %v, %v", allowed, err)
	}
	var retained int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_access_sessions WHERE app_id=$1`, app.ID).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("sessions retained = %d, %v", retained, err)
	}
	if _, err = store.SetAdminAppAccess(ctx, adminID, "admin", app.ID, AppAccessPrivate, "Contest review again"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, session); err != nil || allowed {
		t.Fatalf("old session revived after private switch = %v, %v", allowed, err)
	}
	if _, err = store.SetAdminAppAccess(ctx, adminID, "admin", app.ID, AppAccessPrivate, "No change"); err != nil {
		t.Fatal(err)
	}
	apps, err := store.ListAdminApps(ctx, adminID)
	if err != nil || len(apps) != 1 || apps[0].ID != app.ID || apps[0].AccessMode != AppAccessPrivate {
		t.Fatalf("admin app list = %+v, %v", apps, err)
	}
	if _, err = store.ListAdminApps(ctx, studentID); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("student app list = %v", err)
	}
	audit, err := store.ListAdminAppAccessAudit(ctx, adminID, 100)
	if err != nil || len(audit) != 3 || audit[0].PreviousMode != AppAccessPublic ||
		audit[0].NewMode != AppAccessPrivate || audit[0].Reason != "Contest review again" {
		t.Fatalf("access audit = %+v, %v", audit, err)
	}
}
