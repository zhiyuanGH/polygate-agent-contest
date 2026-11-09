package server

import (
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"polygate/internal/source"
)

func TestSourceBuildLogRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := source.BuildLogPath(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	store := &buildStoreForTest{MemoryStore: NewMemoryStore(), root: root, sourceEnabled: true,
		queued: []SourceBuildTask{{ID: id, ArtifactID: id, AccountID: "student-a", State: "Leased"}}}
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "student-a"}, nil
	}, "", 0, nil, false, root)
	result := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
			"/api/v1/source/builds/"+id+"/log?offset=0", nil))
		result <- response.Code
	}()
	select {
	case code := <-result:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("FIFO log returned HTTP %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO build log blocked the API handler")
	}
}
