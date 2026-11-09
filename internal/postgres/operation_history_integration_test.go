package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"polygate/internal/server"
)

func TestOperationHistoryPaginationAndAccountScope(t *testing.T) {
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
	a, _ := server.NewID()
	b, _ := server.NewID()
	for _, id := range []string{a, b} {
		if _, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
			VALUES($1,$2,$2,'History Test')`, id, "history-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{a, b})
	}()
	for i := 0; i < 55; i++ {
		id, _ := server.NewID()
		if _, err = store.Pool.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,created_at)
			VALUES($1,$2,'history-test','TEST_EVENT','Succeeded',clock_timestamp()-($3::integer * interval '1 second'))`,
			id, a, i); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := server.NewID()
	if _, err = store.Pool.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state)
		VALUES($1,$2,'history-test','OTHER_EVENT','Succeeded')`, other, b); err != nil {
		t.Fatal(err)
	}
	first, err := store.ListOperationHistory(ctx, a, "")
	if err != nil || len(first.Items) != 50 || first.NextCursor != first.Items[49].ID {
		t.Fatalf("first page: len=%d cursor=%q err=%v", len(first.Items), first.NextCursor, err)
	}
	second, err := store.ListOperationHistory(ctx, a, first.NextCursor)
	if err != nil || len(second.Items) != 5 || second.NextCursor != "" {
		t.Fatalf("second page: len=%d cursor=%q err=%v", len(second.Items), second.NextCursor, err)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] || item.ID == other {
			t.Fatalf("duplicate or foreign operation: %s", item.ID)
		}
		seen[item.ID] = true
	}
	if len(seen) != 55 {
		t.Fatalf("paginated account history has %d unique items", len(seen))
	}
	foreign, err := store.ListOperationHistory(ctx, a, other)
	if err != nil || len(foreign.Items) != 0 {
		t.Fatalf("foreign account cursor returned %d items: %v", len(foreign.Items), err)
	}
}
