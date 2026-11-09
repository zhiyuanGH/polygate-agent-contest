package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestOperationHistoryAPIValidatesCursorAndPaginates(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	for i := 0; i < 55; i++ {
		id, _ := NewID()
		store.operations["local-student"] = append(store.operations["local-student"],
			Operation{ID: id, Kind: "TEST_EVENT", State: "Succeeded", CreatedAt: now.Add(-time.Duration(i) * time.Second)})
	}
	foreign, _ := NewID()
	store.operations["other-student"] = []Operation{{ID: foreign, Kind: "FOREIGN_EVENT", CreatedAt: now}}
	handler := NewDevHandler(store)
	request := func(before string) (int, OperationPage) {
		t.Helper()
		path := "/api/v1/operations/history"
		if before != "" {
			path += "?before=" + url.QueryEscape(before)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		var page OperationPage
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return response.Code, page
	}
	if status, _ := request("invalid"); status != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", status)
	}
	status, first := request("")
	if status != http.StatusOK || len(first.Items) != 50 || first.NextCursor != first.Items[49].ID {
		t.Fatalf("first page: %d %+v", status, first)
	}
	status, second := request(first.NextCursor)
	if status != http.StatusOK || len(second.Items) != 5 || second.NextCursor != "" {
		t.Fatalf("second page: %d %+v", status, second)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] || item.ID == foreign {
			t.Fatalf("duplicate or foreign operation: %s", item.ID)
		}
		seen[item.ID] = true
	}
	if status, page := request(foreign); status != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("foreign cursor: status=%d page=%+v", status, page)
	}
}
