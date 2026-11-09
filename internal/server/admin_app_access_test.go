package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testAdminAppID = "22222222-2222-4222-8222-222222222222"

type adminAppAccessTestStore struct {
	*adminAccountTestStore
	changes int
}

func (s *adminAppAccessTestStore) ListAdminApps(context.Context, string) ([]AdminApp, error) {
	return []AdminApp{{ID: testAdminAppID, Name: "sample", AccessMode: "Public"}}, nil
}
func (s *adminAppAccessTestStore) ListAdminAppAccessAudit(context.Context, string, int) ([]AdminAppAccessAudit, error) {
	return []AdminAppAccessAudit{}, nil
}
func (s *adminAppAccessTestStore) SetAdminAppAccess(_ context.Context, _, _, appID, mode, _ string) (AdminApp, error) {
	s.changes++
	return AdminApp{ID: appID, AccessMode: mode}, nil
}

func TestAdminAppAccessRequiresAdminAndReadyPrivateRoute(t *testing.T) {
	store := &adminAppAccessTestStore{adminAccountTestStore: &adminAccountTestStore{MemoryStore: NewMemoryStore()}}
	role := "student"
	h := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "actor", Role: role}, nil
	}).(*Handler)
	path := "/api/v1/admin/apps/" + testAdminAppID + "/access"
	private := `{"mode":"Owner and administrators","reason":"Contest review"}`
	request := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, path, bytes.NewBufferString(body)))
		return w
	}
	if w := request(private); w.Code != http.StatusForbidden {
		t.Fatalf("student changed app access: %d", w.Code)
	}
	role = "admin"
	if w := request(private); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing private route checker = %d", w.Code)
	}
	h.SetPrivateRouteChecker(func(context.Context, string) error { return errors.New("route absent") })
	if w := request(private); w.Code != http.StatusConflict || store.changes != 0 {
		t.Fatalf("unready route changed app access: %d, changes=%d", w.Code, store.changes)
	}
	h.SetPrivateRouteChecker(func(_ context.Context, appID string) error {
		if appID != testAdminAppID {
			t.Errorf("preflight app ID = %s", appID)
		}
		return nil
	})
	for _, invalid := range []string{
		`{"mode":"Owner and administrators","reason":"x"}`,
		`{"mode":"Private","reason":"Contest review"}`,
		`{"mode":"Public","reason":"Contest review","extra":true}`,
	} {
		if w := request(invalid); w.Code != http.StatusBadRequest {
			t.Errorf("invalid access change = %d: %s", w.Code, w.Body.String())
		}
	}
	if w := request(private); w.Code != http.StatusOK || store.changes != 1 {
		t.Fatalf("ready private route = %d, changes=%d", w.Code, store.changes)
	}
	h.SetPrivateRouteChecker(nil)
	if w := request(`{"mode":"Public","reason":"Contest review"}`); w.Code != http.StatusOK || store.changes != 2 {
		t.Fatalf("public recovery = %d, changes=%d", w.Code, store.changes)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/apps", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("admin app list = %d", w.Code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPrivateRoutePreflightRequiresExactGatewayMarker(t *testing.T) {
	marker := testAdminAppID
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://"+testAdminAppID+".polylink.org/_polygate/route-check" {
			t.Errorf("route check URL = %s", req.URL.String())
		}
		header := make(http.Header)
		header.Set("X-PolyGate-Route-App-ID", marker)
		return &http.Response{StatusCode: http.StatusNoContent, Header: header, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}
	check := newPrivateRouteChecker("polylink.org", client)
	if err := check(context.Background(), testAdminAppID); err != nil {
		t.Fatalf("correct route marker rejected: %v", err)
	}
	marker = "other-app"
	if err := check(context.Background(), testAdminAppID); err == nil {
		t.Fatal("wrong gateway marker accepted")
	}
}
