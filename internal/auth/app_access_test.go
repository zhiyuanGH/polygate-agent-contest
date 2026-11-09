package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"polygate/internal/domain"
	"polygate/internal/postgres"
	"polygate/internal/server"
)

const testAccessAppID = "11111111-2222-4333-8444-555555555555"

type fakeTicketIssuer struct{}

func (fakeTicketIssuer) IssueAppAccessTicket(context.Context, string, string, string) (string, error) {
	return "ticket", nil
}

func TestAppLoginReturnPathRejectsExternalAndUnrelatedDestinations(t *testing.T) {
	good := "/auth/app?app=" + testAccessAppID + "&return=%2Fdocs"
	if got := appLoginReturnPath(good); got != good {
		t.Fatalf("app return path = %q", got)
	}
	for _, value := range []string{
		"//evil.example/auth/app?app=" + testAccessAppID,
		"https://evil.example/auth/app",
		"/auth/app/extra?app=" + testAccessAppID,
		"/auth/app\\evil?app=" + testAccessAppID,
		"/api/v1/me",
	} {
		if got := appLoginReturnPath(value); got != "/" {
			t.Errorf("unsafe login next %q became %q", value, got)
		}
	}
}

func TestAppAccessBrokerRejectsBadTargetBeforeLogin(t *testing.T) {
	svc := &Service{secure: true}
	handler := svc.AppAccessHandler(fakeTicketIssuer{}, "polylink.org")
	good := "/auth/app?app=" + testAccessAppID + "&return=%2Fdocs"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", good, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("anonymous browser = %d", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil || location.Path != "/auth/login" || location.Query().Get("next") != good {
		t.Fatalf("login continuation = %q, %v", w.Header().Get("Location"), err)
	}
	for _, raw := range []string{
		"/auth/app?app=invalid", "/auth/app?app=" + testAccessAppID + "&return=%2F%2Fevil.example",
		"/auth/app?app=" + testAccessAppID + "&return=%2F%5Cevil.example",
	} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", raw, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid broker target %q = %d", raw, w.Code)
		}
	}
}

func TestAppAccessBrokerChecksOwnerAndIssuesPrivateHostTicket(t *testing.T) {
	databaseURL := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &postgres.Store{Pool: pool}
	ownerID, _ := server.NewID()
	otherID, _ := server.NewID()
	adminID, _ := server.NewID()
	ids := []string{ownerID, otherID, adminID}
	for _, row := range []struct{ id, role string }{{ownerID, "student"}, {otherID, "student"}, {adminID, "admin"}} {
		if _, err = pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
			VALUES($1,$2,$2,$2,$3)`, row.id, "broker-"+row.id, row.role); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM auth_sessions WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(context.Background(), `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(context.Background(), `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, ids)
	}()
	spec := domain.AppSpec{Name: "broker-test", Image: "example.com/app:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	app, _, err := store.Create(ctx, ownerID, "owner", "broker-test", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE apps SET access_mode=$2 WHERE id=$1`, app.ID, postgres.AppAccessPrivate); err != nil {
		t.Fatal(err)
	}
	svc := &Service{pool: pool, secure: true, baseOrigin: "https://polygate.polylink.org",
		csrfKey: []byte("0123456789abcdef0123456789abcdef")}
	handler := svc.AppAccessHandler(store, "polylink.org")
	requestFor := func(accountID string) *http.Request {
		token := "broker-session-" + accountID
		if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(session_hash,account_id,csrf_hash,expires_at)
			VALUES($1,$2,$3,now()+interval '1 hour')`, tokenHash(token), accountID,
			tokenHash(svc.csrfToken(token))); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/auth/app?app="+app.ID+"&return=%2Fdocs", nil)
		r.AddCookie(&http.Cookie{Name: svc.sessionCookieName(), Value: token})
		return r
	}
	other := httptest.NewRecorder()
	handler.ServeHTTP(other, requestFor(otherID))
	if other.Code != http.StatusForbidden {
		t.Fatalf("other student = %d", other.Code)
	}
	ownerAppSession := ""
	for _, accountID := range []string{ownerID, adminID} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, requestFor(accountID))
		location, err := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusFound || err != nil || location.Host != app.ID+".polylink.org" ||
			location.Path != "/_polygate/auth/consume" || location.Query().Get("ticket") == "" {
			t.Fatalf("authorized ticket redirect = %d %q, %v", w.Code, w.Header().Get("Location"), err)
		}
		session, returnPath, err := store.ConsumeAppAccessTicket(ctx, app.ID, location.Query().Get("ticket"))
		if err != nil || !strings.HasPrefix(returnPath, "/docs") || len(session) != 43 {
			t.Fatalf("ticket validation = %q, %q, %v", session, returnPath, err)
		}
		if accountID == ownerID {
			ownerAppSession = session
		}
	}
	ownerConsoleSession := "broker-session-" + ownerID
	logout := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	logout.Header.Set("Origin", svc.baseOrigin)
	logout.Header.Set("X-CSRF-Token", svc.csrfToken(ownerConsoleSession))
	logout.AddCookie(&http.Cookie{Name: svc.sessionCookieName(), Value: ownerConsoleSession})
	w := httptest.NewRecorder()
	svc.Logout(w, logout)
	if w.Code != http.StatusOK {
		t.Fatalf("owner logout = %d %s", w.Code, w.Body.String())
	}
	if allowed, err := store.AppAccessSessionAllowed(ctx, app.ID, ownerAppSession); err != nil || allowed {
		t.Fatalf("app session survived console logout = %v, %v", allowed, err)
	}
}
