package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type imageRetryStoreForTest struct {
	*MemoryStore
	result ImageArtifact
	replay bool
	err    error
	calls  int
	args   []string
}

func (s *imageRetryStoreForTest) QueueImageScanRetry(_ context.Context, account, subject, artifact, key string) (ImageArtifact, bool, error) {
	s.calls++
	s.args = []string{account, subject, artifact, key}
	return s.result, s.replay, s.err
}

func TestImageScanRetryHTTPContract(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, test := range []struct {
		name, account, imageID, key, body string
		replay                            bool
		err                               error
		status                            int
		code                              string
		called                            bool
	}{
		{name: "queue", account: "owner", imageID: id, key: "retry-key-123", status: http.StatusAccepted, called: true},
		{name: "replay", account: "owner", imageID: id, key: "retry-key-123", replay: true, status: http.StatusOK, called: true},
		{name: "anonymous", imageID: id, key: "retry-key-123", status: http.StatusUnauthorized, code: "UNAUTHENTICATED"},
		{name: "invalid image", account: "owner", imageID: "not-an-id", key: "retry-key-123", status: http.StatusBadRequest, code: "INVALID_IMAGE_ID"},
		{name: "missing key", account: "owner", imageID: id, status: http.StatusBadRequest, code: "INVALID_IDEMPOTENCY_KEY"},
		{name: "invalid key", account: "owner", imageID: id, key: "retry with spaces", status: http.StatusBadRequest, code: "INVALID_IDEMPOTENCY_KEY"},
		{name: "unexpected body", account: "owner", imageID: id, key: "retry-key-123", body: `{"reference":"other/image"}`, status: http.StatusBadRequest, code: "INVALID_SCAN_RETRY_REQUEST"},
		{name: "oversized body", account: "owner", imageID: id, key: "retry-key-123", body: strings.Repeat("x", 257), status: http.StatusBadRequest, code: "INVALID_SCAN_RETRY_REQUEST"},
		{name: "foreign image", account: "other", imageID: id, key: "retry-key-123", err: ErrNotFound, status: http.StatusNotFound, code: "NOT_FOUND", called: true},
		{name: "disabled owner", account: "owner", imageID: id, key: "retry-key-123", err: ErrDisabled, status: http.StatusForbidden, code: "ACCOUNT_DISABLED", called: true},
		{name: "not failed", account: "owner", imageID: id, key: "retry-key-123", err: ErrImageScanNotRetryable, status: http.StatusConflict, code: "IMAGE_SCAN_NOT_RETRYABLE", called: true},
		{name: "cleanup", account: "owner", imageID: id, key: "retry-key-123", err: ErrImageCleanupInProgress, status: http.StatusConflict, code: "IMAGE_CLEANUP_IN_PROGRESS", called: true},
		{name: "key reused", account: "owner", imageID: id, key: "retry-key-123", err: ErrImageScanRetryKeyReused, status: http.StatusConflict, code: "IDEMPOTENCY_KEY_REUSED", called: true},
		{name: "internal error", account: "owner", imageID: id, key: "retry-key-123", err: errors.New("private-upstream-detail"), status: http.StatusInternalServerError, code: "INTERNAL_ERROR", called: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &imageRetryStoreForTest{MemoryStore: NewMemoryStore(), replay: test.replay, err: test.err,
				result: ImageArtifact{ID: id, ScanStatus: "PendingScan", ScanTaskState: "Queued"}}
			h := NewHandler(store, func(r *http.Request) (Principal, error) {
				return Principal{AccountID: r.Header.Get("X-Account"), Subject: "actor"}, nil
			}).(*Handler)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/images/"+test.imageID+"/retry-scan", strings.NewReader(test.body))
			r.Header.Set("X-Account", test.account)
			r.Header.Set("Idempotency-Key", test.key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.status, w.Body.String())
			}
			if (store.calls > 0) != test.called {
				t.Fatalf("store calls = %d, expected called=%v", store.calls, test.called)
			}
			if test.called && (store.args[0] != test.account || store.args[1] != "actor" || store.args[2] != id || store.args[3] != test.key) {
				t.Fatalf("incorrect authenticated retry arguments: %#v", store.args)
			}
			if test.code != "" {
				if !strings.Contains(w.Body.String(), `"code":"`+test.code+`"`) {
					t.Fatalf("missing error code %s: %s", test.code, w.Body.String())
				}
			} else {
				var item ImageArtifact
				if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil || item.ID != id || item.ScanStatus != "PendingScan" || item.ScanTaskState != "Queued" {
					t.Fatalf("unexpected artifact response: %+v %v", item, err)
				}
			}
			if strings.Contains(w.Body.String(), "private-upstream-detail") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("retry response leaked internal error details or was cacheable")
			}
		})
	}
}

func TestImageScanRetryUnavailableWithoutDurableStore(t *testing.T) {
	h := NewHandler(NewMemoryStore(), func(*http.Request) (Principal, error) {
		return Principal{AccountID: "owner"}, nil
	}).(*Handler)
	r := httptest.NewRequest(http.MethodPost, "/retry", nil)
	r.SetPathValue("id", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	r.Header.Set("Idempotency-Key", "retry-key-123")
	w := httptest.NewRecorder()
	h.retryImageScan(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "IMAGE_SCAN_RETRY_UNAVAILABLE") {
		t.Fatalf("unexpected unavailable response: %d %s", w.Code, w.Body.String())
	}
}
