package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"polygate/internal/domain"
	"polygate/internal/secretcatalog"
)

type secretStub struct{ values map[string]map[string]string }

func (s *secretStub) List(_ context.Context, account string) ([]string, error) {
	keys := make([]string, 0, len(s.values[account]))
	for key := range s.values[account] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}
func (s *secretStub) HasKeys(_ context.Context, account string, keys []string) (bool, error) {
	for _, key := range keys {
		if _, ok := s.values[account][key]; !ok {
			return false, nil
		}
	}
	return true, nil
}
func (s *secretStub) PutMarked(_ context.Context, account, key, value, _ string) error {
	if value == "" {
		return secretcatalog.ErrInvalidValue
	}
	if s.values[account] == nil {
		s.values[account] = map[string]string{}
	}
	s.values[account][key] = value
	return nil
}
func (s *secretStub) DeleteMarked(_ context.Context, account, key, _ string) error {
	if _, ok := s.values[account][key]; !ok {
		return secretcatalog.ErrMissing
	}
	delete(s.values[account], key)
	return nil
}

func TestSecretAPIOnlyReturnsNamesAndProtectsReferencedKeys(t *testing.T) {
	store := NewMemoryStore()
	stub := &secretStub{values: map[string]map[string]string{}}
	h := NewHandlerWithAppLogsAndSecrets(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "test"}, nil
	}, "", 0, nil, false, "", false, nil, stub)
	request := func(account, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("X-Test-Account", account)
		if method == http.MethodPost {
			req.Header.Set("Idempotency-Key", "secret-app-key")
		}
		h.ServeHTTP(w, req)
		return w
	}
	const a = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const b = "d2da6205-93d4-4089-a599-c145b7744fe8"
	if w := request(a, "PUT", "/api/v1/secrets/MODEL_API_KEY", `{"value":"private-token"}`); w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("private-token")) {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := request(a, "GET", "/api/v1/secrets", ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("MODEL_API_KEY")) || bytes.Contains(w.Body.Bytes(), []byte("private-token")) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := request(b, "GET", "/api/v1/secrets", ""); w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("MODEL_API_KEY")) {
		t.Fatalf("cross-account list: %d %s", w.Code, w.Body.String())
	}
	if w := request(a, "PUT", "/api/v1/secrets/POLYGATE_INTERNAL", `{"value":"x"}`); w.Code != 400 {
		t.Fatalf("reserved name: %d", w.Code)
	}
	if w := request(a, "PUT", "/api/v1/secrets/BAD", `{"value":""}`); w.Code != 400 {
		t.Fatalf("empty value: %d", w.Code)
	}
	spec := domain.AppSpec{Name: "secret-agent", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}, SecretKeys: []string{"MODEL_API_KEY"}}
	input, _ := json.Marshal(spec)
	if w := request(a, "POST", "/api/v1/apps", string(input)); w.Code != 201 || bytes.Contains(w.Body.Bytes(), []byte("private-token")) {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := request(a, "DELETE", "/api/v1/secrets/MODEL_API_KEY", ""); w.Code != 409 {
		t.Fatalf("delete referenced key: %d %s", w.Code, w.Body.String())
	}
	if w := request(b, "DELETE", "/api/v1/secrets/MODEL_API_KEY", ""); w.Code != 404 {
		t.Fatalf("cross-account delete: %d %s", w.Code, w.Body.String())
	}
	if w := request(b, "PUT", "/api/v1/secrets/OTHER_TOKEN", `{"value":"other-private-token"}`); w.Code != 200 {
		t.Fatalf("second account put: %d %s", w.Code, w.Body.String())
	}
	if w := request(b, "DELETE", "/api/v1/secrets/OTHER_TOKEN", ""); w.Code != 200 {
		t.Fatalf("second account delete: %d %s", w.Code, w.Body.String())
	}
	for _, account := range []string{a, b} {
		if w := request(account, "GET", "/api/v1/operations", ""); w.Code != 200 ||
			bytes.Contains(w.Body.Bytes(), []byte("private-token")) || !bytes.Contains(w.Body.Bytes(), []byte("SECRET_")) {
			t.Fatalf("secret audit leaked value or is missing: %d %s", w.Code, w.Body.String())
		}
	}
	spec.Name, spec.SecretKeys = "missing-key-agent", []string{"ABSENT"}
	input, _ = json.Marshal(spec)
	if w := request(a, "POST", "/api/v1/apps", string(input)); w.Code != 400 {
		t.Fatalf("missing key: %d %s", w.Code, w.Body.String())
	}
}

func TestSecretAppCreationDisabledWithoutCatalog(t *testing.T) {
	spec := domain.AppSpec{Name: "secret-agent", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}, SecretKeys: []string{"MODEL_API_KEY"}}
	input, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(input))
	req.Header.Set("Idempotency-Key", "secret-app-key")
	NewDevHandler(NewMemoryStore()).ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled feature: %d %s", w.Code, w.Body.String())
	}
}
