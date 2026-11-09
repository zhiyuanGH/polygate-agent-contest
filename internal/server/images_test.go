package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"polygate/internal/registry"
)

type imageTestStore struct {
	*MemoryStore
	mu    sync.Mutex
	items map[string][]ImageArtifact
}

type imageDeleteTestStore struct {
	*MemoryStore
	account string
	id      string
}

func (s *imageDeleteTestStore) RequestImageDeletion(_ context.Context, account, _, id string) error {
	s.account, s.id = account, id
	if account != "student-a" {
		return ErrNotFound
	}
	return nil
}

func TestImageDeletionRequiresOwnAccountAndValidID(t *testing.T) {
	store := &imageDeleteTestStore{MemoryStore: NewMemoryStore()}
	h := NewHandler(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "subject"}, nil
	})
	id := "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		account, target string
		want            int
	}{
		{"", id, http.StatusUnauthorized},
		{"student-a", "invalid", http.StatusBadRequest},
		{"student-b", id, http.StatusNotFound},
		{"student-a", id, http.StatusAccepted},
	} {
		r := httptest.NewRequest(http.MethodDelete, "/api/v1/images/"+tc.target, nil)
		r.Header.Set("X-Test-Account", tc.account)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("delete account=%q id=%q: %d %s", tc.account, tc.target, w.Code, w.Body.String())
		}
	}
	if store.account != "student-a" || store.id != id {
		t.Fatalf("deletion scope mismatch: %q %q", store.account, store.id)
	}
}

func (s *imageTestStore) SaveImageArtifact(_ context.Context, account, _ string, image registry.Image) (ImageArtifact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.items[account] {
		if existing.PinnedReference == image.PinnedReference {
			return existing, true, nil
		}
	}
	item := ImageArtifact{ID: "artifact-" + account, SubmittedReference: image.SubmittedReference,
		PinnedReference: image.PinnedReference, PlatformDigest: image.PlatformDigest,
		RegistryHost: image.RegistryHost, ScanStatus: "PendingScan", CreatedAt: time.Now()}
	s.items[account] = append(s.items[account], item)
	return item, false, nil
}

func (s *imageTestStore) ListImageArtifacts(_ context.Context, account string) ([]ImageArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ImageArtifact{}, s.items[account]...), nil
}

func imageRequest(h http.Handler, account, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/images/inspect", strings.NewReader(body))
	r.Header.Set("X-Test-Account", account)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestImageInspectionIsAccountScopedAndPendingScan(t *testing.T) {
	store := &imageTestStore{MemoryStore: NewMemoryStore(), items: make(map[string][]ImageArtifact)}
	image := registry.Image{SubmittedReference: "registry.example/agent:v1",
		PinnedReference: "registry.example/agent@sha256:" + strings.Repeat("a", 64),
		PlatformDigest:  "sha256:" + strings.Repeat("b", 64), RegistryHost: "registry.example"}
	inspections := 0
	h := NewHandlerWithImageInspection(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "subject"}, nil
	}, "", 0, func(_ context.Context, ref string) (registry.Image, error) {
		inspections++
		if ref != image.SubmittedReference {
			t.Errorf("reference = %q", ref)
		}
		return image, nil
	})
	for _, tc := range []struct {
		account string
		want    int
	}{
		{"student-a", http.StatusCreated},
		{"student-a", http.StatusOK},
		{"student-b", http.StatusCreated},
	} {
		w := imageRequest(h, tc.account, `{"reference":"registry.example/agent:v1"}`)
		if w.Code != tc.want || !bytes.Contains(w.Body.Bytes(), []byte(`"scanStatus":"PendingScan"`)) {
			t.Fatalf("inspect as %s: %d %s", tc.account, w.Code, w.Body.String())
		}
	}
	if inspections != 3 {
		t.Fatalf("inspections = %d", inspections)
	}
	for _, account := range []string{"student-a", "student-b", ""} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/images", nil)
		r.Header.Set("X-Test-Account", account)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if account == "" {
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous list: %d", w.Code)
			}
			continue
		}
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("artifact-"+account)) ||
			bytes.Contains(w.Body.Bytes(), []byte("artifact-"+map[string]string{"student-a": "student-b", "student-b": "student-a"}[account])) {
			t.Fatalf("list as %s: %d %s", account, w.Code, w.Body.String())
		}
	}
}

func TestImageInspectionFailsClosedAndHidesRegistryErrors(t *testing.T) {
	store := &imageTestStore{MemoryStore: NewMemoryStore(), items: make(map[string][]ImageArtifact)}
	auth := func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "subject"}, nil
	}
	if got := imageRequest(NewHandler(store, auth), "student-a", `{"reference":"registry.example/a:v1"}`).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("disabled inspection = %d", got)
	}
	h := NewHandlerWithImageInspection(store, auth, "", 0, func(context.Context, string) (registry.Image, error) {
		return registry.Image{}, errors.New("secret-token=xyz private-signed-url")
	})
	for _, body := range []string{`{}`, `{"reference":"x","accountId":"victim"}`, `{"reference":"x"} {}`, strings.Repeat("x", 2049)} {
		if got := imageRequest(h, "student-a", body).Code; got != http.StatusBadRequest {
			t.Fatalf("invalid request = %d", got)
		}
	}
	if got := imageRequest(h, "", `{"reference":"registry.example/a:v1"}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous inspection = %d", got)
	}
	w := imageRequest(h, "student-a", `{"reference":"registry.example/a:v1"}`)
	if w.Code != http.StatusBadRequest || bytes.Contains(w.Body.Bytes(), []byte("secret-token")) ||
		bytes.Contains(w.Body.Bytes(), []byte("private-signed-url")) {
		t.Fatalf("registry error leaked: %d %s", w.Code, w.Body.String())
	}
}
