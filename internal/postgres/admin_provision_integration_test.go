package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"polygate/internal/server"
)

func TestAccountProvisionSlotRetryAndAudit(t *testing.T) {
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
	slotID, _ := server.NewID()
	if _, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
		VALUES($1,$2,$2,'Test Admin','admin')`, adminID, adminID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,enabled,provisioning_state)
		VALUES($1,$2,$3,'Available account slot',false,'Available')`, slotID, slotID, "slot-"+slotID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM admin_account_audit WHERE target_account_id=$1`, slotID)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id IN ($1,$2)`, adminID, slotID)
	}()
	id, claim, err := store.ClaimAccountSlot(ctx, adminID, "test-student", "Test Student")
	if err != nil || id != slotID {
		t.Fatalf("claim: %s %v", id, err)
	}
	if _, _, err = store.ClaimAccountSlot(ctx, adminID, "test-student", "Test Student"); err == nil {
		t.Fatal("concurrent claim accepted")
	}
	if _, err = store.FinishAccountProvision(ctx, adminID, "admin-sub", slotID, "00000000-0000-4000-8000-000000000000", "33333333-3333-4333-8333-333333333333", "Test enrollment"); err == nil {
		t.Fatal("wrong claim finalized")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE accounts SET provision_claimed_at=now()-interval '6 minutes' WHERE id=$1`, slotID); err != nil {
		t.Fatal(err)
	}
	_, nextClaim, err := store.ClaimAccountSlot(ctx, adminID, "test-student", "Test Student")
	if err != nil || nextClaim == claim {
		t.Fatalf("stale claim: %v", err)
	}
	identityID := "33333333-3333-4333-8333-333333333333"
	account, err := store.FinishAccountProvision(ctx, adminID, "admin-sub", slotID, nextClaim, identityID, "Test enrollment")
	if err != nil || !account.Enabled || account.Username != "test-student" {
		t.Fatalf("finish: %+v %v", account, err)
	}
	if _, _, err = store.ClaimAccountSlot(ctx, adminID, "test-student", "Test Student"); err == nil {
		t.Fatal("active username reclaimed")
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO auth_sessions(session_hash,account_id,csrf_hash,expires_at)
		VALUES(decode(md5($1::text),'hex'),$1::uuid,decode('aabbcc','hex'),now()+interval '1 hour')`, slotID); err != nil {
		t.Fatal(err)
	}
	if subject, err := store.StudentForPasswordReset(ctx, adminID, slotID); err != nil || subject != identityID {
		t.Fatalf("identity subject: %s %v", subject, err)
	}
	var sessions int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE account_id=$1`, slotID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("password reset did not revoke sessions: %d %v", sessions, err)
	}
	if err = store.RecordPasswordReset(ctx, adminID, "admin-sub", slotID, "Test reset"); err != nil {
		t.Fatal(err)
	}
	var audit int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_account_audit WHERE target_account_id=$1`, slotID).Scan(&audit); err != nil || audit != 2 {
		t.Fatalf("audit count: %d %v", audit, err)
	}
}
