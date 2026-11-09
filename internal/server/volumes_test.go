package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"polygate/internal/domain"
)

func TestVolumeReservationAPIIsAccountScopedAndQuotaBound(t *testing.T) {
	t.Setenv("POLYGATE_VOLUME_ENABLED", "1")
	store := NewMemoryStore()
	handler := func(account string) http.Handler {
		return NewHandler(store, func(*http.Request) (Principal, error) {
			return Principal{AccountID: account, Subject: account, Role: "student"}, nil
		})
	}
	a, b := handler("account-a"), handler("account-b")
	post := func(h http.Handler, key string, spec domain.VolumeSpec) *httptest.ResponseRecorder {
		body, _ := json.Marshal(spec)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/volumes", bytes.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first := post(a, "volume-first-key", domain.VolumeSpec{Name: "data", SizeGiB: 5})
	if first.Code != http.StatusCreated {
		t.Fatalf("first reservation: %d %s", first.Code, first.Body.String())
	}
	var item Volume
	if err := json.Unmarshal(first.Body.Bytes(), &item); err != nil || item.State != "Reserved" {
		t.Fatalf("reservation response: %+v %v", item, err)
	}
	if replay := post(a, "volume-first-key", item.Spec); replay.Code != http.StatusOK {
		t.Fatalf("idempotent replay: %d %s", replay.Code, replay.Body.String())
	}
	if reused := post(a, "volume-first-key", domain.VolumeSpec{Name: "data", SizeGiB: 6}); reused.Code != http.StatusBadRequest {
		t.Fatalf("changed replay: %d", reused.Code)
	}
	if duplicate := post(a, "volume-other-key", item.Spec); duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name: %d", duplicate.Code)
	}
	if second := post(a, "volume-second-key", domain.VolumeSpec{Name: "cache", SizeGiB: 5}); second.Code != http.StatusCreated {
		t.Fatalf("second reservation: %d %s", second.Code, second.Body.String())
	}
	if third := post(a, "volume-third-key", domain.VolumeSpec{Name: "third", SizeGiB: 1}); third.Code != http.StatusBadRequest {
		t.Fatalf("count quota: %d", third.Code)
	}
	if cross := post(b, "volume-first-key", item.Spec); cross.Code != http.StatusCreated {
		t.Fatalf("account B independent reservation: %d", cross.Code)
	}
	for _, path := range []string{"/api/v1/volumes", "/api/v1/volumes/" + item.ID} {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/api/v1/volumes" {
			if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(item.ID)) {
				t.Fatalf("account B list leaked A: %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != http.StatusNotFound {
			t.Fatalf("account B can read A volume: %d", w.Code)
		}
	}
}

func TestVolumeDeletionRequiresExactNameAndUnboundReadyVolume(t *testing.T) {
	t.Setenv("POLYGATE_VOLUME_ENABLED", "1")
	store := NewMemoryStore()
	handler := NewHandler(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "account-a", Subject: "student-a", Role: "student"}, nil
	})
	item, _, err := store.CreateVolume(t.Context(), "account-a", "student-a", "delete-volume-key", domain.VolumeSpec{Name: "agent-data", SizeGiB: 2})
	if err != nil {
		t.Fatal(err)
	}
	store.volumes["account-a"][item.ID] = Volume{ID: item.ID, Spec: item.Spec, State: "Ready", CreatedAt: item.CreatedAt}
	deleteRequest := func(name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"confirmName": name})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/volumes/"+item.ID, bytes.NewReader(body)))
		return w
	}
	if w := deleteRequest("wrong-name"); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation: %d %s", w.Code, w.Body.String())
	}
	store.apps["account-a"] = map[string]App{"app-a": {ID: "app-a", Spec: domain.AppSpec{VolumeID: item.ID}}}
	if w := deleteRequest(item.Spec.Name); w.Code != http.StatusBadRequest {
		t.Fatalf("bound volume deletion: %d %s", w.Code, w.Body.String())
	}
	delete(store.apps["account-a"], "app-a")
	if w := deleteRequest(item.Spec.Name); w.Code != http.StatusAccepted {
		t.Fatalf("confirmed deletion: %d %s", w.Code, w.Body.String())
	}
	if w := deleteRequest(item.Spec.Name); w.Code != http.StatusAccepted {
		t.Fatalf("idempotent deletion: %d %s", w.Code, w.Body.String())
	}
	if got := store.volumes["account-a"][item.ID]; got.State != "Deleting" {
		t.Fatalf("volume state: %+v", got)
	}
	if len(store.operations["account-a"]) != 2 {
		t.Fatalf("deletion replay created another operation: %+v", store.operations["account-a"])
	}
}
