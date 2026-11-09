package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type provisionTestStore struct {
	*MemoryStore
	claimed, finished, reset int
}

func (s *provisionTestStore) ClaimAccountSlot(context.Context, string, string, string) (string, string, error) {
	s.claimed++
	return "22222222-2222-4222-8222-222222222222", "claim", nil
}
func (s *provisionTestStore) FinishAccountProvision(context.Context, string, string, string, string, string, string) (AdminAccount, error) {
	s.finished++
	return AdminAccount{ID: "22222222-2222-4222-8222-222222222222", Username: "contestant"}, nil
}
func (s *provisionTestStore) StudentForPasswordReset(context.Context, string, string) (string, error) {
	return "22222222-2222-4222-8222-222222222222", nil
}
func (s *provisionTestStore) RecordPasswordReset(context.Context, string, string, string, string) error {
	s.reset++
	return nil
}

type provisionTestManager struct{ created, reset, enabled, logout int }

func (m *provisionTestManager) CreateTemporaryUser(_ context.Context, _, _, _, password string) (string, bool, error) {
	if len(password) < 20 {
		panic("short password")
	}
	m.created++
	return "33333333-3333-4333-8333-333333333333", true, nil
}
func (m *provisionTestManager) ResetTemporaryPassword(_ context.Context, _, password string) error {
	if len(password) < 20 {
		panic("short password")
	}
	m.reset++
	return nil
}
func (m *provisionTestManager) SetEnabled(context.Context, string, bool) error {
	m.enabled++
	return nil
}
func (m *provisionTestManager) LogoutUser(context.Context, string) error { m.logout++; return nil }

func TestAccountProvisionRequiresAdminAndReturnsPasswordOnce(t *testing.T) {
	store := &provisionTestStore{MemoryStore: NewMemoryStore()}
	manager := &provisionTestManager{}
	role := "student"
	handler := NewAccountAdministrationHandler(http.NotFoundHandler(), store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "admin-sub", Role: role}, nil
	}, manager)
	call := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w
	}
	createPath := "/api/v1/admin/accounts"
	createBody := `{"username":"contestant","displayName":"Contest Student","reason":"Contest enrollment"}`
	if w := call(createPath, createBody); w.Code != http.StatusForbidden || store.claimed != 0 {
		t.Fatalf("student creation: %d", w.Code)
	}
	role = "admin"
	if w := call(createPath, `{"username":"bad name","displayName":"Student","reason":"Contest enrollment"}`); w.Code != http.StatusBadRequest || store.claimed != 0 {
		t.Fatalf("invalid username: %d", w.Code)
	}
	w := call(createPath, createBody)
	if w.Code != http.StatusCreated || store.claimed != 1 || store.finished != 1 || manager.created != 1 || manager.enabled != 1 {
		t.Fatalf("creation: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		TemporaryPassword string `json:"temporaryPassword"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.TemporaryPassword) < 32 {
		t.Fatal("one-time password missing")
	}
	resetPath := "/api/v1/admin/accounts/22222222-2222-4222-8222-222222222222/reset-password"
	if w := call(resetPath, `{"reason":"Admin reset"}`); w.Code != http.StatusOK || store.reset != 1 || manager.reset != 1 || manager.logout != 1 {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
}
