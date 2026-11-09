package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"polygate/internal/registry"
)

type imageUploadTestStore struct{ *imageTestStore }

func (s *imageUploadTestStore) Account(ctx context.Context, id string) (Account, error) {
	account, err := s.MemoryStore.Account(ctx, id)
	account.ImageDeployEnabled = true
	return account, err
}

func TestImageTarUploadIsAccountScopedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	store := &imageUploadTestStore{&imageTestStore{MemoryStore: NewMemoryStore(), items: map[string][]ImageArtifact{}}}
	owner := "student-a"
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: owner, Subject: owner}, nil
	}, "", 0, nil, true, root).(*Handler)
	archive := []byte("docker-save-test-content")
	called := 0
	h.SetImageUploader(func(_ context.Context, account, id, path string) (registry.Image, error) {
		called++
		if account != "student-a" || id == "" {
			t.Fatal("uploader identity mismatch")
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, archive) {
			t.Fatalf("assembled image: %q %v", data, err)
		}
		pinned := "harbor.polygate-registry.svc.cluster.local/account/agent@sha256:" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		return registry.Image{SubmittedReference: pinned, PinnedReference: pinned,
			PlatformDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RegistryHost:   "harbor.polygate-registry.svc.cluster.local"}, nil
	})
	created := httptest.NewRecorder()
	h.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v1/images/uploads",
		bytes.NewBufferString(`{"sizeBytes":`+strconv.Itoa(len(archive))+`}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var upload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &upload); err != nil || upload.ID == "" {
		t.Fatalf("upload ID: %+v %v", upload, err)
	}
	base := "/api/v1/images/uploads/" + upload.ID
	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, base+"/complete", nil))
	if missing.Code != http.StatusConflict {
		t.Fatalf("missing chunk: %d %s", missing.Code, missing.Body.String())
	}
	owner = "student-b"
	foreign := httptest.NewRecorder()
	h.ServeHTTP(foreign, httptest.NewRequest(http.MethodPut, base+"/chunks/0", bytes.NewReader(archive)))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign upload: %d %s", foreign.Code, foreign.Body.String())
	}
	owner = "student-a"
	chunk := httptest.NewRecorder()
	h.ServeHTTP(chunk, httptest.NewRequest(http.MethodPut, base+"/chunks/0", bytes.NewReader(archive)))
	if chunk.Code != http.StatusOK {
		t.Fatalf("chunk: %d %s", chunk.Code, chunk.Body.String())
	}
	complete := httptest.NewRecorder()
	h.ServeHTTP(complete, httptest.NewRequest(http.MethodPost, base+"/complete", nil))
	if complete.Code != http.StatusCreated || called != 1 {
		t.Fatalf("complete: %d %s", complete.Code, complete.Body.String())
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, httptest.NewRequest(http.MethodPost, base+"/complete", nil))
	if replay.Code != http.StatusOK || called != 1 {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
}
