package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"polygate/internal/applogs"
	"polygate/internal/domain"
)

type fakeAppLogReader struct {
	calls        int
	emptyMetrics bool
}

func (r *fakeAppLogReader) Read(_ context.Context, _, _, pod string) (applogs.Result, error) {
	r.calls++
	if pod == "foreign-pod" {
		return applogs.Result{}, applogs.ErrPodNotFound
	}
	return applogs.Result{Pod: "owned-pod", Data: "hello\n"}, nil
}

func (r *fakeAppLogReader) List(_ context.Context, _, _ string) ([]applogs.PodInfo, error) {
	r.calls++
	return []applogs.PodInfo{{Name: "owned-pod", Phase: "Running", Ready: true}}, nil
}

func (r *fakeAppLogReader) Events(_ context.Context, _, _ string) ([]applogs.Event, error) {
	r.calls++
	return []applogs.Event{{Reason: "Scheduled", Object: "owned-pod"}}, nil
}

func (r *fakeAppLogReader) Metrics(_ context.Context, _, _ string) ([]applogs.PodMetric, error) {
	r.calls++
	if r.emptyMetrics {
		return []applogs.PodMetric{}, nil
	}
	return []applogs.PodMetric{{Pod: "owned-pod", CPUMilli: 25, MemoryBytes: 1048576}}, nil
}

func TestAccountActualUsageIsTenantScopedAndMarksMissingSamples(t *testing.T) {
	store := NewMemoryStore()
	spec := domain.AppSpec{Name: "agent", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	for _, account := range []string{"student-a", "student-b"} {
		app, _, err := store.Create(context.Background(), account, "sub", account+"-app-key", spec)
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		app.Status = "Running"
		store.apps[account][app.ID] = app
		store.mu.Unlock()
	}
	reader := &fakeAppLogReader{}
	h := NewHandlerWithAppLogs(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "test"}, nil
	}, "", 0, nil, false, "", false, reader)
	request := func() map[string]any {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/usage/actual", nil)
		r.Header.Set("X-Test-Account", "student-a")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("account metrics status=%d body=%s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	got := request()
	if got["cpuMilli"] != float64(25) || got["memoryBytes"] != float64(1048576) ||
		got["sampledPods"] != float64(1) || got["expectedPods"] != float64(1) ||
		got["complete"] != true || reader.calls != 1 {
		t.Fatalf("account metrics leaked or differ: %+v calls=%d", got, reader.calls)
	}
	reader.emptyMetrics = true
	got = request()
	if got["complete"] != false || got["sampledPods"] != float64(0) || reader.calls != 2 {
		t.Fatalf("missing sample reported as complete: %+v calls=%d", got, reader.calls)
	}
}

func (r *fakeAppLogReader) History(_ context.Context, _, _ string, _ int, before, pod string) (applogs.HistoryResult, error) {
	r.calls++
	if before == "bad" {
		return applogs.HistoryResult{}, applogs.ErrInvalidHistoryCursor
	}
	if pod == "foreign-pod" {
		return applogs.HistoryResult{}, applogs.ErrInvalidHistoryPod
	}
	return applogs.HistoryResult{Items: []applogs.HistoryLine{{Line: "owned history"}}, RangeHours: 24}, nil
}

func TestAppLogsRequireOwnedAppBeforeClusterRead(t *testing.T) {
	store := NewMemoryStore()
	spec := domain.AppSpec{Name: "agent", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	app, _, err := store.Create(context.Background(), "student-a", "sub-a", "app-log-test-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeAppLogReader{}
	h := NewHandlerWithAppLogs(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "test"}, nil
	}, "", 0, nil, false, "", false, reader)
	request := func(account, suffix string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/logs"+suffix, nil)
		req.Header.Set("X-Test-Account", account)
		h.ServeHTTP(w, req)
		return w
	}
	if w := request("student-b", ""); w.Code != http.StatusNotFound || reader.calls != 0 {
		t.Fatalf("foreign app leaked: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	if w := request("student-a", "?pod=foreign-pod"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign pod accepted: status=%d", w.Code)
	}
	if w := request("student-a", ""); w.Code != http.StatusOK || reader.calls != 2 {
		t.Fatalf("owned app log failed: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	for _, tc := range []struct {
		account string
		status  int
	}{{"student-b", 404}, {"student-a", 200}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/pods", nil)
		req.Header.Set("X-Test-Account", tc.account)
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("pods as %s: %d", tc.account, w.Code)
		}
	}
	if reader.calls != 3 {
		t.Fatalf("foreign Pod list request reached cluster: %d calls", reader.calls)
	}
	for _, tc := range []struct {
		account string
		status  int
	}{{"student-b", 404}, {"student-a", 200}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/events", nil)
		req.Header.Set("X-Test-Account", tc.account)
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("events as %s: %d", tc.account, w.Code)
		}
	}
	if reader.calls != 4 {
		t.Fatalf("foreign event request reached cluster: %d calls", reader.calls)
	}
	for _, tc := range []struct {
		account string
		status  int
	}{{"student-b", 404}, {"student-a", 200}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/metrics", nil)
		req.Header.Set("X-Test-Account", tc.account)
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("metrics as %s: %d", tc.account, w.Code)
		}
	}
	if reader.calls != 5 {
		t.Fatalf("foreign metrics request reached cluster: %d calls", reader.calls)
	}
	if w := request("student-b", "/history"); w.Code != http.StatusNotFound || reader.calls != 5 {
		t.Fatalf("foreign history leaked: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	if w := request("student-a", "/history?hours=72"); w.Code != http.StatusBadRequest || reader.calls != 5 {
		t.Fatalf("invalid history range accepted: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	if w := request("student-a", "/history?hours=24"); w.Code != http.StatusOK || reader.calls != 6 {
		t.Fatalf("owned app history failed: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	if w := request("student-a", "/history?hours=24&before=bad"); w.Code != http.StatusBadRequest || reader.calls != 7 {
		t.Fatalf("invalid history cursor accepted: status=%d readerCalls=%d", w.Code, reader.calls)
	}
	if w := request("student-a", "/history?hours=24&pod=foreign-pod"); w.Code != http.StatusBadRequest || reader.calls != 8 {
		t.Fatalf("invalid history pod accepted: status=%d readerCalls=%d", w.Code, reader.calls)
	}
}
