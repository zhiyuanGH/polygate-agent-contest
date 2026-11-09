package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"polygate/internal/domain"
)

func zipForTest(t *testing.T, name string) []byte {
	t.Helper()
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	w, err := z.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("FROM scratch\n")); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestAppDraftAPI(t *testing.T) {
	h := NewDevHandler(NewMemoryStore())
	valid := domain.AppSpec{Name: "agent-a", Image: "registry.example/agent:v1", Port: 8080, Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 1}}
	body, _ := json.Marshal(valid)
	key := "create-one-key"
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		h.ServeHTTP(w, req)
		return w
	}
	if got := request().Code; got != http.StatusCreated {
		t.Fatalf("create status = %d", got)
	}
	if got := request().Code; got != http.StatusOK {
		t.Fatalf("idempotent replay status = %d", got)
	}
	key = "duplicate-name-key"
	if got := request().Code; got != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d", got)
	}
	valid.Name = "agent-b"
	body, _ = json.Marshal(valid)
	key = "create-two-key"
	w := request()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("quota status = %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Error domain.Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "QUOTA_EXCEEDED" || result.Error.Field != "memoryMiB" {
		t.Fatalf("wrong quota error: %+v", result.Error)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"Draft"`)) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
}

func TestDraftResourceEditAPIUpdatesConfiguredQuotaAndAudit(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	spec := domain.AppSpec{Name: "agent-edit", Image: "example.com/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, "local-student", "student", "resource-edit-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	h := NewDevHandler(store)
	patch := func(value domain.Resources) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"resources": value})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID+"/resources", bytes.NewReader(body)))
		return w
	}
	changed := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 2}
	if w := patch(changed); w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	stored, err := store.Get(ctx, "local-student", app.ID)
	if err != nil || stored.Spec.Resources != changed {
		t.Fatalf("stored edit: %+v %v", stored, err)
	}
	_, usage, err := store.List(ctx, "local-student")
	if err != nil || usage.MemoryMiB != 2048 || usage.Pods != 2 {
		t.Fatalf("configured usage: %+v %v", usage, err)
	}
	if w := patch(domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 2}); w.Code != http.StatusBadRequest {
		t.Fatalf("over quota edit: %d %s", w.Code, w.Body.String())
	}
	if w := patch(changed); w.Code != http.StatusOK {
		t.Fatalf("repeat edit: %d %s", w.Code, w.Body.String())
	}
	operations, err := store.ListOperations(ctx, "local-student")
	if err != nil || len(operations) != 2 || operations[0].Kind != "UPDATE_APP_RESOURCES" {
		t.Fatalf("resource audit: %+v %v", operations, err)
	}
}

func TestFailedAppRuntimeEditCanRepairPortWithoutChangingQuota(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	spec := domain.AppSpec{Name: "python-sample", Image: "example.com/python:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, "local-student", "student", "runtime-edit-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	app.Status = "Failed"
	store.apps["local-student"][app.ID] = app
	h := NewDevHandler(store)
	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch,
			"/api/v1/apps/"+app.ID+"/runtime", strings.NewReader(body)))
		return w
	}
	if w := patch(`{"port":8000,"healthPath":"/health"}`); w.Code != http.StatusOK {
		t.Fatalf("repair failed app: %d %s", w.Code, w.Body.String())
	}
	stored, err := store.Get(ctx, "local-student", app.ID)
	if err != nil || stored.Spec.Port != 8000 || stored.Spec.HealthPath != "/health" {
		t.Fatalf("saved runtime settings: %+v %v", stored, err)
	}
	if w := patch(`{"port":0,"healthPath":"/health"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid port accepted: %d %s", w.Code, w.Body.String())
	}
	stored.Status = "Running"
	store.apps["local-student"][app.ID] = stored
	if w := patch(`{"port":3000,"healthPath":""}`); w.Code == http.StatusOK {
		t.Fatal("running app runtime settings changed without rollout")
	}
	_, usage, err := store.List(ctx, "local-student")
	if err != nil || usage.MemoryMiB != 512 || usage.Pods != 1 {
		t.Fatalf("runtime edit changed quota: %+v %v", usage, err)
	}
	operations, err := store.ListOperations(ctx, "local-student")
	if err != nil || len(operations) != 2 || operations[0].Kind != "UPDATE_APP_RUNTIME" {
		t.Fatalf("runtime edit audit: %+v %v", operations, err)
	}
}

func TestSourceValidationRejectsTraversal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		zipBytes []byte
		want     int
	}{
		{"bad ZIP", []byte("not a zip"), 400},
		{"valid ZIP", zipForTest(t, "Dockerfile"), 200},
		{"path traversal", zipForTest(t, "../outside"), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			mw := multipart.NewWriter(&data)
			part, err := mw.CreateFormFile("source", "source.zip")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write(tc.zipBytes)
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/source/validate", &data)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			NewDevHandler(NewMemoryStore()).ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestConcurrentCreatesCannotExceedAccountQuota(t *testing.T) {
	store := NewMemoryStore()
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := store.Create(context.Background(), "student-a", "student-a", fmt.Sprintf("key-%d-unique", i), domain.AppSpec{
				Name: fmt.Sprintf("app-%d", i), Image: "example.com/agent:v1", Port: 8080,
				Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1},
			})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted %d apps, want 2", accepted)
	}
}

func TestAccountIsolation(t *testing.T) {
	store := NewMemoryStore()
	spec := domain.AppSpec{Name: "agent", Image: "example.com/agent:v1", Port: 8080, Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	a, _, err := store.Create(context.Background(), "student-a", "sub-a", "key-for-a", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(context.Background(), "student-b", "sub-b", "key-for-b", spec); err != nil {
		t.Fatal("same app name in another account should work:", err)
	}
	h := NewHandler(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Test-Account"), Subject: "test"}, nil
	})
	for _, tc := range []struct {
		account string
		method  string
		path    string
		status  int
	}{
		{"student-a", "GET", "/api/v1/apps/" + a.ID, 200},
		{"student-b", "GET", "/api/v1/apps/" + a.ID, 404},
		{"student-a", "GET", "/api/v1/apps/" + a.ID + "/status", 200},
		{"student-b", "GET", "/api/v1/apps/" + a.ID + "/status", 404},
		{"student-a", "GET", "/api/v1/apps/" + a.ID + "/revisions", 200},
		{"student-b", "GET", "/api/v1/apps/" + a.ID + "/revisions", 404},
		{"student-a", "GET", "/api/v1/apps/" + a.ID + "/revisions?before=-1", 400},
		{"student-a", "GET", "/api/v1/apps/" + a.ID + "/revisions?before=2147483648", 400},
		{"student-b", "DELETE", "/api/v1/apps/" + a.ID, 404},
		{"", "GET", "/api/v1/apps", 401},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Test-Account", tc.account)
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("%s %s as %s: got %d, want %d", tc.method, tc.path, tc.account, w.Code, tc.status)
		}
	}
}

type testDeployStore struct {
	*MemoryStore
	account       string
	appID         string
	digest        string
	port          int
	calls         int
	stopCalls     int
	imageCalls    int
	updateCalls   int
	restartCalls  int
	strategy      string
	rollbackCalls int
	artifactID    string
}

func (s *testDeployStore) QueueTrustedDeploy(_ context.Context, account, _, appID, _, digest string, port int) (DeploymentTask, bool, error) {
	s.account, s.appID, s.digest, s.port = account, appID, digest, port
	s.calls++
	return DeploymentTask{ID: "task-id", AppID: appID, State: "Queued"}, s.calls > 1, nil
}

func (s *testDeployStore) QueueStop(_ context.Context, account, _, appID, _ string) (DeploymentTask, bool, error) {
	s.account, s.appID = account, appID
	s.stopCalls++
	return DeploymentTask{ID: "stop-task", AppID: appID, DesiredState: "Stopped", State: "Queued"}, s.stopCalls > 1, nil
}

func (s *testDeployStore) QueueApprovedImageDeploy(_ context.Context, account, _, appID, _, artifactID string) (DeploymentTask, bool, error) {
	s.account, s.appID, s.artifactID = account, appID, artifactID
	s.imageCalls++
	return DeploymentTask{ID: "image-task", AppID: appID, State: "Queued"}, s.imageCalls > 1, nil
}

func (s *testDeployStore) QueueApprovedImageUpdate(_ context.Context, account, _, appID, _, artifactID string) (DeploymentTask, bool, error) {
	s.account, s.appID, s.artifactID = account, appID, artifactID
	s.updateCalls++
	return DeploymentTask{ID: "update-task", AppID: appID, State: "Queued"}, s.updateCalls > 1, nil
}

func (s *testDeployStore) QueueRunningRestart(_ context.Context, account, _, appID, _, strategy string) (DeploymentTask, bool, error) {
	s.account, s.appID, s.strategy = account, appID, strategy
	s.restartCalls++
	return DeploymentTask{ID: "restart-task", AppID: appID, State: "Queued"}, s.restartCalls > 1, nil
}

func TestRestartAPIIsAccountScopedAndValidatesStrategy(t *testing.T) {
	store := &testDeployStore{MemoryStore: NewMemoryStore()}
	authenticate := func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "subject-a"}, nil
	}
	h := NewHandler(store, authenticate)
	request := func(body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/restart", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "restart-key-001")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(`{"strategy":"Rolling","namespace":"kube-system"}`); got != http.StatusBadRequest || store.restartCalls != 0 {
		t.Fatalf("restart accepted student namespace: status=%d calls=%d", got, store.restartCalls)
	}
	if got := request(`{"strategy":"Unknown"}`); got != http.StatusBadRequest || store.restartCalls != 0 {
		t.Fatalf("restart accepted unknown strategy: status=%d calls=%d", got, store.restartCalls)
	}
	if got := request(`{"strategy":"Recreate"}`); got != http.StatusAccepted ||
		store.account != "student-a" || store.appID != "app-123" || store.strategy != "Recreate" {
		t.Fatalf("restart request: status=%d account=%s app=%s strategy=%s", got, store.account, store.appID, store.strategy)
	}
	if got := request(`{"strategy":"Recreate"}`); got != http.StatusOK {
		t.Fatalf("restart replay status=%d", got)
	}
}

func (s *testDeployStore) QueueApprovedRevisionRollback(_ context.Context, account, _, appID, _, revisionID string) (DeploymentTask, bool, error) {
	s.account, s.appID, s.artifactID = account, appID, revisionID
	s.rollbackCalls++
	return DeploymentTask{ID: "rollback-task", AppID: appID, State: "Queued"}, s.rollbackCalls > 1, nil
}

func TestRevisionRollbackAPIUsesAuthenticatedAccountAndRevisionID(t *testing.T) {
	store := &testDeployStore{MemoryStore: NewMemoryStore()}
	h := NewHandlerWithImageDeployment(store, func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "subject-a"}, nil
	}, "", 0, nil, true)
	request := func(body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/rollback", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "rollback-key-001")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(`{"revisionId":"revision-a","artifactId":"arbitrary"}`); got != http.StatusBadRequest || store.rollbackCalls != 0 {
		t.Fatalf("unexpected rollback field accepted: status=%d calls=%d", got, store.rollbackCalls)
	}
	if got := request(`{"revisionId":"revision-a"}`); got != http.StatusAccepted ||
		store.account != "student-a" || store.appID != "app-123" || store.artifactID != "revision-a" {
		t.Fatalf("rollback request: status=%d account=%s app=%s revision=%s", got, store.account, store.appID, store.artifactID)
	}
	if got := request(`{"revisionId":"revision-a"}`); got != http.StatusOK {
		t.Fatalf("rollback replay status=%d", got)
	}
}

func TestApprovedImageUpdateAPIIsGatedAndAccountScoped(t *testing.T) {
	store := &testDeployStore{MemoryStore: NewMemoryStore()}
	authenticate := func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "subject-a"}, nil
	}
	request := func(h http.Handler, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/update-image", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "update-image-001")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(NewHandlerWithImageInspection(store, authenticate, "", 0, nil), `{"artifactId":"artifact-a"}`); got != http.StatusServiceUnavailable || store.updateCalls != 0 {
		t.Fatalf("disabled update: status=%d calls=%d", got, store.updateCalls)
	}
	h := NewHandlerWithImageDeployment(store, authenticate, "", 0, nil, true)
	if got := request(h, `{"artifactId":"artifact-a","image":"untrusted.example/app:latest"}`); got != http.StatusBadRequest || store.updateCalls != 0 {
		t.Fatalf("student image substitution accepted: status=%d calls=%d", got, store.updateCalls)
	}
	if got := request(h, `{"artifactId":"artifact-a"}`); got != http.StatusAccepted || store.account != "student-a" || store.appID != "app-123" || store.artifactID != "artifact-a" {
		t.Fatalf("approved update: status=%d account=%s app=%s artifact=%s", got, store.account, store.appID, store.artifactID)
	}
	if got := request(h, `{"artifactId":"artifact-a"}`); got != http.StatusOK {
		t.Fatalf("update replay status=%d", got)
	}
}

func TestApprovedImageDeployAPIIsExplicitlyGated(t *testing.T) {
	store := &testDeployStore{MemoryStore: NewMemoryStore()}
	authenticate := func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "subject-a"}, nil
	}
	request := func(h http.Handler, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/deploy-image",
			bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "approved-image-001")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(NewHandlerWithImageInspection(store, authenticate, "", 0, nil),
		`{"artifactId":"artifact-a"}`); got != http.StatusServiceUnavailable || store.imageCalls != 0 {
		t.Fatalf("disabled image deployment: status=%d calls=%d", got, store.imageCalls)
	}
	h := NewHandlerWithImageDeployment(store, authenticate, "", 0, nil, true)
	if got := request(h, `{"artifactId":"artifact-a","image":"untrusted.example/app:latest"}`); got != http.StatusBadRequest || store.imageCalls != 0 {
		t.Fatalf("student-supplied runtime image accepted: status=%d calls=%d", got, store.imageCalls)
	}
	if got := request(h, `{"artifactId":"artifact-a"}`); got != http.StatusAccepted ||
		store.account != "student-a" || store.appID != "app-123" || store.artifactID != "artifact-a" {
		t.Fatalf("approved image request: status=%d account=%s app=%s artifact=%s",
			got, store.account, store.appID, store.artifactID)
	}
	if got := request(h, `{"artifactId":"artifact-a"}`); got != http.StatusOK {
		t.Fatalf("approved image replay status=%d", got)
	}
}

func TestTrustedDeployAPIIsGatedAndAcceptsNoStudentConfiguration(t *testing.T) {
	store := &testDeployStore{MemoryStore: NewMemoryStore()}
	authenticate := func(*http.Request) (Principal, error) {
		return Principal{AccountID: "student-a", Subject: "subject-a"}, nil
	}
	request := func(h http.Handler, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/deploy", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "deploy-key-001")
		h.ServeHTTP(w, r)
		return w
	}
	if got := request(NewHandler(store, authenticate), "").Code; got != http.StatusServiceUnavailable || store.calls != 0 {
		t.Fatalf("disabled deployment API: status=%d calls=%d", got, store.calls)
	}
	h := NewHandlerWithTrustedImage(store, authenticate, "platform.example/image@sha256:approved", 5678)
	if got := request(h, `{"namespace":"kube-system","image":"student-image"}`).Code; got != http.StatusBadRequest || store.calls != 0 {
		t.Fatalf("student deployment settings accepted: status=%d calls=%d", got, store.calls)
	}
	if got := request(h, "").Code; got != http.StatusAccepted || store.account != "student-a" ||
		store.appID != "app-123" || store.digest != "platform.example/image@sha256:approved" || store.port != 5678 {
		t.Fatalf("trusted deployment request: status=%d account=%s app=%s digest=%s",
			got, store.account, store.appID, store.digest)
	}
	if got := request(h, "").Code; got != http.StatusOK {
		t.Fatalf("idempotent deployment replay status=%d", got)
	}
	stop := func(h http.Handler, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/app-123/stop", bytes.NewBufferString(body))
		r.Header.Set("Idempotency-Key", "stop-key-001")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := stop(NewHandler(NewMemoryStore(), authenticate), ""); got != http.StatusServiceUnavailable || store.stopCalls != 0 {
		t.Fatalf("disabled stop API: status=%d calls=%d", got, store.stopCalls)
	}
	if got := stop(h, `{"namespace":"other"}`); got != http.StatusBadRequest || store.stopCalls != 0 {
		t.Fatalf("student stop settings accepted: status=%d calls=%d", got, store.stopCalls)
	}
	if got := stop(h, ""); got != http.StatusAccepted || store.stopCalls != 1 {
		t.Fatalf("stop request: status=%d calls=%d", got, store.stopCalls)
	}
	if got := stop(h, ""); got != http.StatusOK {
		t.Fatalf("idempotent stop replay status=%d", got)
	}
	imageOnly := &testDeployStore{MemoryStore: NewMemoryStore()}
	if got := stop(NewHandlerWithImageDeployment(imageOnly, authenticate, "", 0, nil, true), ""); got != http.StatusAccepted || imageOnly.stopCalls != 1 {
		t.Fatalf("approved image stop without sample config: status=%d calls=%d", got, imageOnly.stopCalls)
	}
}
