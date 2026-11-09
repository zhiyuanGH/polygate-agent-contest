package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminClusterResourcesRequiresAdmin(t *testing.T) {
	role := "student"
	h := NewHandler(NewMemoryStore(), func(*http.Request) (Principal, error) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Role: role}, nil
	})
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cluster-resources", nil))
		return w
	}
	if w := request(); w.Code != http.StatusForbidden {
		t.Fatalf("student received %d: %s", w.Code, w.Body.String())
	}
	role = "admin"
	t.Setenv("POLYGATE_PROMETHEUS_URL", "")
	if w := request(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing Prometheus URL returned %d: %s", w.Code, w.Body.String())
	}
}
