package applogs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	result, err := reader.History(context.Background(), account, app, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Line != "owned line" || result.Items[0].Pod != "owned" {
		t.Fatalf("history returned foreign or missing data: %+v", result)
	}
}
