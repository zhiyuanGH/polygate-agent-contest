package applogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestHistoryQueriesOnlyOwnedLabelsAndFiltersResponse(t *testing.T) {
	account := "fb056bb4-cf9f-4faa-af71-5aad4c10b46b"
	app := "bfaa67a4-cfaf-48d5-a047-b9cbb15bb147"
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("query") != `{account_id="`+account+`",app_id="`+app+`"}` ||
			q.Get("limit") != "200" || q.Get("direction") != "backward" {
			t.Errorf("unsafe or unbounded Loki query: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[`+
			`{"stream":{"account_id":%q,"app_id":%q,"pod":"owned"},"values":[[%q,"owned line"]]},`+
			`{"stream":{"account_id":"foreign","app_id":%q,"pod":"foreign"},"values":[[%q,"secret"]]}`+
			`]}}`, account, app, stamp, app, stamp)
	}))
	defer server.Close()
	reader := &Reader{lokiURL: server.URL, lokiHTTP: server.Client()}
	result, err := reader.History(context.Background(), account, app, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Line != "owned line" || result.Items[0].Pod != "owned" {
		t.Fatalf("history returned foreign or missing data: %+v", result)
	}
}

func TestHistoryPagesDoNotRepeatBoundaryLine(t *testing.T) {
	const account = "fb056bb4-cf9f-4faa-af71-5aad4c10b46b"
	const app = "bfaa67a4-cfaf-48d5-a047-b9cbb15bb147"
	newest := time.Now().Add(-time.Hour).UnixNano()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		end, err := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		if err != nil {
			t.Errorf("invalid end timestamp: %v", err)
		}
		values := make([][]string, 0, 200)
		if end >= newest-199 {
			for index := int64(0); index < 200; index++ {
				values = append(values, []string{strconv.FormatInt(newest-index, 10), "line"})
			}
		} else {
			values = append(values, []string{strconv.FormatInt(newest-200, 10), "older"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"result": []any{map[string]any{"stream": map[string]string{
				"account_id": account, "app_id": app, "pod": "owned"}, "values": values}},
		}})
	}))
	defer server.Close()
	reader := &Reader{lokiURL: server.URL, lokiHTTP: server.Client()}
	first, err := reader.History(context.Background(), account, app, 24, "")
	if err != nil || len(first.Items) != 200 || first.NextBefore != strconv.FormatInt(newest-199, 10) {
		t.Fatalf("first page incomplete: count=%d cursor=%s err=%v", len(first.Items), first.NextBefore, err)
	}
	second, err := reader.History(context.Background(), account, app, 24, first.NextBefore)
	if err != nil || len(second.Items) != 1 || second.Items[0].Line != "older" || second.NextBefore != "" {
		t.Fatalf("second page repeated or missing: %+v err=%v", second, err)
	}
	if _, err := reader.History(context.Background(), account, app, 24, "not-a-cursor"); !errors.Is(err, ErrInvalidHistoryCursor) {
		t.Fatalf("invalid cursor accepted: %v", err)
	}
}
