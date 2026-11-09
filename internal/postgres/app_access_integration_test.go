package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestAppAccessTicketsAndSessionsStayBoundToOwnerAndApp(t *testing.T) {
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
	ownerID, _ := server.NewID()
	otherID, _ := server.NewID()
	adminID, _ := server.NewID()
	ids := []string{ownerID, otherID, adminID}
	for _, row := range []struct{ id, role string }{
		{ownerID, "student"}, {otherID, "student"}, {adminID, "admin"},
	} {
		_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
			VALUES($1,$2,$2,$2,$3)`, row.id, "app-access-"+row.id, row.role)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, ids)
	}()
	spec := domain.AppSpec{Name: "access-test", Image: "example.com/app:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	app, _, err := store.Create(ctx, ownerID, "owner", "create-app", spec)
	if err != nil {
		t.Fatal(err)
	}
	if app.AccessMode != AppAccessPublic {
		t.Fatalf("new app access mode = %q", app.AccessMode)
	}
	if allowed, err := store.PublicAppAllowed(ctx, app.ID); err != nil || !allowed {
		t.Fatalf("public app route = %v, %v", allowed, err)
	}
	if allowed, err := store.PrivateAppAllowed(ctx, app.ID); err != nil || allowed {
		t.Fatalf("public app private route = %v, %v", allowed, err)
	}
	otherApp, _, err := store.Create(ctx, otherID, "other", "create-other-app", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueAppAccessTicket(ctx, ownerID, app.ID, "/docs"); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("ticket for public app = %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE apps SET access_mode=$2 WHERE id IN ($1,$3)`,
		app.ID, AppAccessPrivate, otherApp.ID); err != nil {
		t.Fatal(err)
	}
	if access, err := store.GetAppAccess(ctx, app.ID); err != nil || access.Mode != AppAccessPrivate || access.OwnerID != ownerID {
		t.Fatalf("app access = %+v, %v", access, err)
	}
	if allowed, err := store.PublicAppAllowed(ctx, app.ID); err != nil || allowed {
		t.Fatalf("private app leaked through public route = %v, %v", allowed, err)
	}
	if allowed, err := store.PrivateAppAllowed(ctx, app.ID); err != nil || !allowed {
		t.Fatalf("private app route = %v, %v", allowed, err)
	}
	if _, err := store.IssueAppAccessTicket(ctx, otherID, app.ID, "/docs"); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("other student ticket = %v", err)
	}
	for _, path := range []string{"//evil.example/path", "/%2Fevil.example/path", "/\\evil.example",
		"/%5Cevil.example", "/_polygate/auth/consume", "/%5Fpolygate/auth/consume"} {
		if _, err := store.IssueAppAccessTicket(ctx, ownerID, app.ID, path); !errors.Is(err, server.ErrForbidden) {
			t.Fatalf("unsafe return path %q = %v", path, err)
		}
	}
	ticket, err := store.IssueAppAccessTicket(ctx, ownerID, app.ID, "/docs?view=full")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConsumeAppAccessTicket(ctx, otherApp.ID, ticket); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("cross-app ticket = %v", err)
	}
	session, returnPath, err := store.ConsumeAppAccessTicket(ctx, app.ID, ticket)
	if err != nil || returnPath != "/docs?view=full" || len(session) != 43 {
		t.Fatalf("consume ticket = %q, %q, %v", session, returnPath, err)
	}
	if _, _, err := store.ConsumeAppAccessTicket(ctx, app.ID, ticket); !errors.Is(err, server.ErrForbidden) {
		t.Fatalf("replayed ticket = %v", err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, session); err != nil || !allowed {
		t.Fatalf("owner session = %v, %v", allowed, err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, otherApp.ID, session); err != nil || allowed {
		t.Fatalf("cross-app session = %v, %v", allowed, err)
	}
	adminTicket, err := store.IssueAppAccessTicket(ctx, adminID, app.ID, "/")
	if err != nil {
		t.Fatal(err)
	}
	adminSession, _, err := store.ConsumeAppAccessTicket(ctx, app.ID, adminTicket)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, adminSession); err != nil || !allowed {
		t.Fatalf("admin session = %v, %v", allowed, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET role='student' WHERE id=$1`, adminID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, adminSession); err != nil || allowed {
		t.Fatalf("demoted admin session = %v, %v", allowed, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=false WHERE id=$1`, ownerID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, session); err != nil || allowed {
		t.Fatalf("disabled owner session = %v, %v", allowed, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=true WHERE id=$1`, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE apps SET access_mode=$2 WHERE id=$1`, app.ID, AppAccessPublic); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, session); err != nil || allowed {
		t.Fatalf("session after public switch = %v, %v", allowed, err)
	}
}

func TestAppAccessTicketOnlyOneConcurrentConsumer(t *testing.T) {
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
	ownerID, _ := server.NewID()
	_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
		VALUES($1,$2,$2,$2)`, ownerID, "ticket-race-"+ownerID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=$1`, ownerID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=$1`, ownerID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, ownerID)
	}()
	spec := domain.AppSpec{Name: "ticket-race", Image: "example.com/app:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	app, _, err := store.Create(ctx, ownerID, "owner", "ticket-race", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE apps SET access_mode=$2 WHERE id=$1`, app.ID, AppAccessPrivate); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.IssueAppAccessTicket(ctx, ownerID, app.ID, "/")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := store.ConsumeAppAccessTicket(ctx, app.ID, ticket)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, server.ErrForbidden) {
			t.Fatalf("concurrent ticket consumption = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful consumers = %d, want 1", successes)
	}
}
