package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type adminAccountTestStore struct {
	*MemoryStore
	changes int
}

func (s *adminAccountTestStore) ListAdminAccounts(context.Context, string) ([]AdminAccount, error) {
	return []AdminAccount{}, nil
}

func (s *adminAccountTestStore) ListAdminAccountAudit(context.Context, string, int) ([]AdminAccountAudit, error) {
	return []AdminAccountAudit{}, nil
}

func (s *adminAccountTestStore) SetAdminAccountEnabled(_ context.Context, _, _, target string, enabled bool, _ string) (AdminAccount, error) {
	s.changes++
	return AdminAccount{ID: target, Enabled: enabled}, nil
}

func TestAdminAccountRoutesRequireAdminAndReason(t *testing.T) {
	store := &adminAccountTestStore{MemoryStore: NewMemoryStore()}
	role := "student"
	h := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "admin-subject", Role: role}, nil
	})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
		return w
	}
	if w := request(http.MethodGet, "/api/v1/admin/accounts", ""); w.Code != http.StatusForbidden {
		t.Fatalf("student list: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPatch, "/api/v1/admin/accounts/22222222-2222-4222-8222-222222222222/enabled", `{"enabled":false,"reason":"test reason"}`); w.Code != http.StatusForbidden {
		t.Fatalf("student write: %d %s", w.Code, w.Body.String())
	}
	role = "admin"
	if w := request(http.MethodGet, "/api/v1/admin/accounts", ""); w.Code != http.StatusOK {
		t.Fatalf("admin list: %d %s", w.Code, w.Body.String())
	}
	path := "/api/v1/admin/accounts/22222222-2222-4222-8222-222222222222/enabled"
	for _, body := range []string{`{"enabled":false}`, `{"enabled":false,"reason":"x"}`, `{"enabled":false,"reason":"valid","unexpected":true}`, `{"reason":"test reason"}`} {
		if w := request(http.MethodPatch, path, body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid body %q: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := request(http.MethodPatch, "/api/v1/admin/accounts/11111111-1111-4111-8111-111111111111/enabled", `{"enabled":false,"reason":"test reason"}`); w.Code != http.StatusForbidden {
		t.Fatalf("self disable: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPatch, path, `{"enabled":false,"reason":"Contest policy violation"}`); w.Code != http.StatusOK {
		t.Fatalf("admin disable: %d %s", w.Code, w.Body.String())
	}
	if store.changes != 1 {
		t.Fatalf("changes = %d", store.changes)
	}
}
