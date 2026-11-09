package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"polygate/internal/domain"
)

type adminQuotaTestStore struct {
	*adminAccountTestStore
	queued int
}

func (s *adminQuotaTestStore) QueueAdminQuotaChange(_ context.Context, _, _, target, _, reason string,
	quota domain.Quota) (QuotaChange, bool, error) {
	s.queued++
	return QuotaChange{ID: "change-id", TargetAccountID: target, Quota: quota, Reason: reason, State: "Queued"}, false, nil
}

func (s *adminQuotaTestStore) ListAdminQuotaChanges(context.Context, string) ([]QuotaChange, error) {
	return []QuotaChange{}, nil
}

func TestAdminQuotaRouteStaysDisabledUntilControllerIsReady(t *testing.T) {
	store := &adminQuotaTestStore{adminAccountTestStore: &adminAccountTestStore{MemoryStore: NewMemoryStore()}}
	h := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "admin-subject", Role: "admin"}, nil
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/quota-changes", nil))
	if w.Code != http.StatusServiceUnavailable || store.queued != 0 {
		t.Fatalf("quota gate: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminQuotaRouteValidatesAndQueues(t *testing.T) {
	t.Setenv("POLYGATE_ADMIN_QUOTA_ENABLED", "1")
	store := &adminQuotaTestStore{adminAccountTestStore: &adminAccountTestStore{MemoryStore: NewMemoryStore()}}
	role := "student"
	h := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "admin-subject", Role: role}, nil
	})
	request := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/accounts/22222222-2222-4222-8222-222222222222/quota", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "quota-test-key")
		h.ServeHTTP(w, r)
		return w
	}
	valid := `{"quota":{"cpuRequestMilli":3000,"cpuLimitMilli":6000,"memoryMiB":8192,"pods":5,"apps":3},"reason":"Contest accommodation"}`
	if w := request(valid); w.Code != http.StatusForbidden {
		t.Fatalf("student request: %d %s", w.Code, w.Body.String())
	}
	role = "admin"
	for _, body := range []string{
		`{"quota":{"cpuRequestMilli":3000,"cpuLimitMilli":6000,"memoryMiB":8192,"pods":5,"apps":3},"reason":"x"}`,
		`{"quota":{"cpuRequestMilli":3000,"cpuLimitMilli":6000,"memoryMiB":999999,"pods":5,"apps":3},"reason":"Contest accommodation"}`,
		`{"quota":{"cpuRequestMilli":3000,"cpuLimitMilli":6000,"memoryMiB":8192,"pods":5,"apps":3},"reason":"Contest accommodation","extra":true}`,
	} {
		if w := request(body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid request: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(valid); w.Code != http.StatusAccepted {
		t.Fatalf("valid request: %d %s", w.Code, w.Body.String())
	}
	if store.queued != 1 {
		t.Fatalf("queued = %d", store.queued)
	}
}

type quotaPreviewTestStore struct {
	*adminQuotaTestStore
	targetRole string
}

func (s *quotaPreviewTestStore) ListAdminAccounts(context.Context, string) ([]AdminAccount, error) {
	return []AdminAccount{{ID: "22222222-2222-4222-8222-222222222222", Role: s.targetRole,
		Quota: domain.StudentQuota}}, nil
}

func (s *quotaPreviewTestStore) List(_ context.Context, accountID string) ([]App, domain.Usage, error) {
	if accountID != "22222222-2222-4222-8222-222222222222" {
		return nil, domain.Usage{}, ErrNotFound
	}
	return []App{{ID: "app-id", Status: "Running", Spec: domain.AppSpec{
			Name: "contest-app", Image: "private.example/secret-image", SecretKeys: []string{"MODEL_KEY"},
			Resources: domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 2048, Replicas: 2}}}},
		domain.Usage{CPURequestMilli: 1000, CPULimitMilli: 2000, MemoryMiB: 4096, Pods: 2, Apps: 1}, nil
}

func TestAdminQuotaPreviewShowsImpactWithoutChangingQuota(t *testing.T) {
	t.Setenv("POLYGATE_ADMIN_QUOTA_ENABLED", "1")
	store := &quotaPreviewTestStore{adminQuotaTestStore: &adminQuotaTestStore{
		adminAccountTestStore: &adminAccountTestStore{MemoryStore: NewMemoryStore()}},
		targetRole: "student"}
	role := "student"
	h := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Subject: "admin-subject", Role: role}, nil
	})
	request := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body)))
		return w
	}
	path := "/api/v1/admin/accounts/22222222-2222-4222-8222-222222222222/quota-preview"
	body := `{"quota":{"cpuRequestMilli":250,"cpuLimitMilli":1000,"memoryMiB":1024,"pods":1,"apps":1}}`
	if w := request(path, body); w.Code != http.StatusForbidden {
		t.Fatalf("student preview: %d %s", w.Code, w.Body.String())
	}
	role = "admin"
	for _, invalid := range []string{`{}`, body[:len(body)-1],
		`{"quota":{"cpuRequestMilli":250,"cpuLimitMilli":1000,"memoryMiB":1024,"pods":1,"apps":1},"extra":true}`} {
		if w := request(path, invalid); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid preview: %d %s", w.Code, w.Body.String())
		}
	}
	w := request(path, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview AdminQuotaPreview
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Configured.MemoryMiB != 4096 || len(preview.OverQuota) != 4 ||
		len(preview.Apps) != 1 || preview.Apps[0].Name != "contest-app" ||
		store.queued != 0 || bytes.Contains(w.Body.Bytes(), []byte("secret-image")) ||
		bytes.Contains(w.Body.Bytes(), []byte("MODEL_KEY")) {
		t.Fatalf("preview leaked details or changed quota: %+v queued=%d", preview, store.queued)
	}
	store.targetRole = "admin"
	if w := request(path, body); w.Code != http.StatusForbidden {
		t.Fatalf("administrator target preview: %d %s", w.Code, w.Body.String())
	}
}
