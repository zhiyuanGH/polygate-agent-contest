package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"polygate/internal/source"
)

type buildStoreForTest struct {
	*MemoryStore
	root          string
	queued        []SourceBuildTask
	sourceEnabled bool
}

func (s *buildStoreForTest) Account(ctx context.Context, id string) (Account, error) {
	account, err := s.MemoryStore.Account(ctx, id)
	account.SourceBuildEnabled = s.sourceEnabled
	return account, err
}

func (s *buildStoreForTest) ReserveSourceBuild(_ context.Context, account, _ string, id string, size int64) (SourceBuildTask, error) {
	task := SourceBuildTask{ID: id, ArtifactID: id, AccountID: account, SizeBytes: size, State: "Preparing"}
	s.queued = append(s.queued, task)
	return task, nil
}
func (s *buildStoreForTest) ActivateSourceBuild(_ context.Context, account, id, digest string) (SourceBuildTask, error) {
	path, err := source.ArchivePath(s.root, id)
	if err != nil {
		return SourceBuildTask{}, err
	}
	data, err := os.ReadFile(path)
	for i := range s.queued {
		if s.queued[i].ArtifactID == id && s.queued[i].AccountID == account {
			if err != nil || int64(len(data)) != s.queued[i].SizeBytes || len(digest) != 71 {
				return SourceBuildTask{}, io.ErrUnexpectedEOF
			}
			s.queued[i].SourceSHA256 = digest
			s.queued[i].State = "Queued"
			return s.queued[i], nil
		}
	}
	return SourceBuildTask{}, ErrNotFound
}
func (s *buildStoreForTest) AbandonSourceBuild(_ context.Context, account, id string) (bool, error) {
	return true, nil
}
func (s *buildStoreForTest) ListSourceBuilds(_ context.Context, account string) ([]SourceBuildTask, error) {
	items := make([]SourceBuildTask, 0)
	for _, task := range s.queued {
		if task.AccountID == account {
			items = append(items, task)
		}
	}
	return items, nil
}
func (s *buildStoreForTest) GetSourceBuild(_ context.Context, account, id string) (SourceBuildTask, error) {
	for _, task := range s.queued {
		if task.AccountID == account && task.ID == id {
			return task, nil
		}
	}
	return SourceBuildTask{}, ErrNotFound
}

func TestSourceBuildSubmissionPersistsValidatedZIP(t *testing.T) {
	root := t.TempDir()
	store := &buildStoreForTest{MemoryStore: NewMemoryStore(), root: root, sourceEnabled: true}
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "student-a"}, nil
	}, "", 0, nil, false, root)
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	f, err := m.CreateFormFile("source", "entry.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(zipForTest(t, "Dockerfile"))
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/source/builds", &body)
	req.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	if len(store.queued) != 1 {
		t.Fatalf("queued %d", len(store.queued))
	}
	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/source/builds", nil))
	if list.Code != http.StatusOK || !bytes.Contains(list.Body.Bytes(), []byte(store.queued[0].ID)) {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	// An unsafe ZIP cannot create an archive or a queued task.
	var invalid bytes.Buffer
	m2 := multipart.NewWriter(&invalid)
	f2, _ := m2.CreateFormFile("source", "bad.zip")
	_, _ = f2.Write(zipForTest(t, "../Dockerfile"))
	_ = m2.Close()
	bad := httptest.NewRequest(http.MethodPost, "/api/v1/source/builds", &invalid)
	bad.Header.Set("Content-Type", m2.FormDataContentType())
	rejected := httptest.NewRecorder()
	h.ServeHTTP(rejected, bad)
	if rejected.Code != http.StatusBadRequest || len(store.queued) != 1 {
		t.Fatalf("invalid ZIP: %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestSourceBuildPythonTemplateIsPersistedAsDockerfile(t *testing.T) {
	root := t.TempDir()
	store := &buildStoreForTest{MemoryStore: NewMemoryStore(), root: root, sourceEnabled: true}
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "student-a"}, nil
	}, "", 0, nil, false, root)
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	f, err := m.CreateFormFile("source", "agent.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(zipForTest(t, "main.py")); err != nil {
		t.Fatal(err)
	}
	if err = m.WriteField("template", "python"); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/source/builds", &body)
	req.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || len(store.queued) != 1 {
		t.Fatalf("template submission: %d %s", w.Code, w.Body.String())
	}
	path, err := source.ArchivePath(root, store.queued[0].ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, member := range z.File {
		if member.Name != "Dockerfile" {
			continue
		}
		stream, err := member.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		found = bytes.Equal(content, []byte(source.PythonBuildTemplate))
	}
	if !found {
		t.Fatal("stored ZIP lacks fixed Python Dockerfile")
	}
	bad := zipForTest(t, "main.py")
	var invalid bytes.Buffer
	m2 := multipart.NewWriter(&invalid)
	f2, _ := m2.CreateFormFile("source", "agent.zip")
	_, _ = f2.Write(bad)
	_ = m2.Close()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/source/builds", &invalid)
	req.Header.Set("Content-Type", m2.FormDataContentType())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || len(store.queued) != 1 {
		t.Fatalf("missing Dockerfile accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestSourceBuildRequiresAccountAdmission(t *testing.T) {
	root := t.TempDir()
	store := &buildStoreForTest{MemoryStore: NewMemoryStore(), root: root}
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "student-a"}, nil
	}, "", 0, nil, false, root)
	me := httptest.NewRecorder()
	h.ServeHTTP(me, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if me.Code != http.StatusOK || !bytes.Contains(me.Body.Bytes(), []byte(`"sourceBuildAvailable":false`)) {
		t.Fatalf("disabled capability: %d %s", me.Code, me.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/source/builds", nil))
		if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("BUILD_NOT_ENABLED")) || len(store.queued) != 0 {
			t.Fatalf("%s admitted source build: %d %s", method, w.Code, w.Body.String())
		}
	}
	store.sourceEnabled = true
	me = httptest.NewRecorder()
	h.ServeHTTP(me, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if me.Code != http.StatusOK || !bytes.Contains(me.Body.Bytes(), []byte(`"sourceBuildAvailable":true`)) {
		t.Fatalf("enabled capability: %d %s", me.Code, me.Body.String())
	}
}

func TestSourceBuildLogIsScopedToOwner(t *testing.T) {
	root := t.TempDir()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	path, err := source.BuildLogPath(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("build output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &buildStoreForTest{MemoryStore: NewMemoryStore(), root: root, sourceEnabled: true,
		queued: []SourceBuildTask{{ID: id, ArtifactID: id, AccountID: "student-a", State: "Leased"}}}
	account := "student-a"
	h := NewHandlerWithBuildSubmission(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: account, Subject: account}, nil
	}, "", 0, nil, false, root)
	url := "/api/v1/source/builds/" + id + "/log?offset=0"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("owner log: %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		Data       string `json:"data"`
		NextOffset int64  `json:"nextOffset"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil || string(data) != "build output\n" || payload.NextOffset != int64(len(data)) {
		t.Fatalf("owner log data: %+v %v", payload, err)
	}
	appendLog, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = appendLog.WriteString("next line\n"); err != nil {
		t.Fatal(err)
	}
	if err = appendLog.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/source/builds/"+id+"/log?offset="+strconv.FormatInt(payload.NextOffset, 10), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("incremental log: %d %s", w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, err = base64.StdEncoding.DecodeString(payload.Data)
	if err != nil || string(data) != "next line\n" || payload.NextOffset != int64(len("build output\nnext line\n")) {
		t.Fatalf("incremental log data: %+v %v", payload, err)
	}
	account = "student-b"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusNotFound || bytes.Contains(w.Body.Bytes(), data) {
		t.Fatalf("cross-account log: %d %s", w.Code, w.Body.String())
	}
	secret := filepath.Join(root, "secret.txt")
	if err = os.WriteFile(secret, []byte("private secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	account = "student-a"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusServiceUnavailable || bytes.Contains(w.Body.Bytes(), []byte("private secret")) {
		t.Fatalf("symlink log: %d %s", w.Code, w.Body.String())
	}
}
