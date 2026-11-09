package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"polygate/internal/server"
)

func TestSessionAccountDisableAndExpiry(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	accountID, _ := server.NewID()
	sessionID := "integration-session-" + accountID
	_, err = pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
		VALUES($1,$2,$2,'Session Student')`, accountID, "session-"+accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID) }()
	svc := &Service{pool: pool, csrfKey: []byte("integration-test-key"), baseOrigin: "http://127.0.0.1:5173"}
	_, err = pool.Exec(ctx, `INSERT INTO auth_sessions(session_hash,account_id,csrf_hash,expires_at)
		VALUES($1,$2,$3,now()+interval '1 hour')`, tokenHash(sessionID), accountID, tokenHash(svc.csrfToken(sessionID)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM auth_sessions WHERE account_id=$1`, accountID)
	}()
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.AddCookie(&http.Cookie{Name: svc.sessionCookieName(), Value: sessionID})
		return r
	}
	if p, err := svc.Current(request()); err != nil || p.AccountID != accountID {
		t.Fatalf("active session: %+v %v", p, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE accounts SET enabled=false WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Current(request()); !errors.Is(err, server.ErrDisabled) {
		t.Fatalf("disabled account = %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE accounts SET enabled=true WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=now()-interval '1 second' WHERE account_id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Current(request()); err == nil || errors.Is(err, server.ErrDisabled) {
		t.Fatalf("expired session = %v", err)
	}
}
