package sourcebuild

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"polygate/internal/buildagent"
	"polygate/internal/harbor"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/server"
)

const accountID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const taskID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
const artifactID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

type fakeStore struct {
	task      postgres.SourceBuildTask
	claims    int
	failed    string
	retry     bool
	completed bool
}

func (f *fakeStore) ClaimNextSourceBuild(context.Context, string, time.Duration) (postgres.SourceBuildTask, bool, error) {
	f.claims++
	return f.task, true, nil
}
func (f *fakeStore) FailSourceBuild(_ context.Context, _, _ string, _ int, code string, retry bool) error {
	f.failed, f.retry = code, retry
	return nil
}
func (f *fakeStore) CompleteLeasedSourceBuild(_ context.Context, _ postgres.SourceBuildTask, _ string, _ registry.Image) (server.ImageArtifact, error) {
	f.completed = true
	return server.ImageArtifact{ID: artifactID}, nil
}

type fakeAgent struct {
	calls    int
	readyErr error
}

func (a *fakeAgent) Ready(context.Context) error { return a.readyErr }

func (a *fakeAgent) Build(_ context.Context, req buildagent.Request) (buildagent.Result, error) {
	a.calls++
	return buildagent.Result{ImageDigest: "sha256:" + strings.Repeat("d", 64),
		LogSHA256: "sha256:" + strings.Repeat("e", 64)}, nil
}

type fakeVerifier struct {
	wrongAccount bool
	projectErr   error
}

func (v fakeVerifier) EnsureBuildProject(context.Context, string) error { return v.projectErr }

func (v fakeVerifier) VerifyBuiltImage(_ context.Context, account, _, digest string) (registry.Image, error) {
	project, _ := harbor.AccountProjectName(account)
	if v.wrongAccount {
		project, _ = harbor.AccountProjectName("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	}
	pinned := harbor.RegistryHost + "/" + project + "/agent@" + digest
	return registry.Image{SubmittedReference: pinned, PinnedReference: pinned,
		ManifestDigest: digest, PlatformDigest: digest, RegistryHost: harbor.RegistryHost,
		OS: "linux", Architecture: "amd64"}, nil
}

func testWorker(t *testing.T) (*Worker, *fakeStore, *fakeAgent) {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("FROM scratch\n"))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.WriteFile(filepath.Join(root, artifactID+".zip"), archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(archive.Bytes())
	store := &fakeStore{task: postgres.SourceBuildTask{ID: taskID, ArtifactID: artifactID,
		AccountID: accountID, SourceSHA256: "sha256:" + hex.EncodeToString(hash[:]),
		SizeBytes: int64(archive.Len()), Attempt: 1}}
	agent := &fakeAgent{}
	return &Worker{Store: store, Agent: agent, Verifier: fakeVerifier{},
		SourceRoot: root, ID: "test-worker"}, store, agent
}

func TestWorkerCompletesOnlyVerifiedAccountImage(t *testing.T) {
	w, store, agent := testWorker(t)
	ok, err := w.RunOnce(context.Background())
	if err != nil || !ok || !store.completed || agent.calls != 1 {
		t.Fatalf("verified build: ok=%v err=%v completed=%v calls=%d", ok, err, store.completed, agent.calls)
	}
	w, store, agent = testWorker(t)
	w.Verifier = fakeVerifier{wrongAccount: true}
	ok, err = w.RunOnce(context.Background())
	if !ok || err == nil || store.completed || store.failed != "BUILD_IMAGE_MISMATCH" || agent.calls != 1 {
		t.Fatalf("cross-account image: ok=%v err=%v failed=%s completed=%v", ok, err, store.failed, store.completed)
	}
}

func TestWorkerRejectsChangedSourceBeforeAgent(t *testing.T) {
	w, store, agent := testWorker(t)
	path := filepath.Join(w.SourceRoot, artifactID+".zip")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	ok, err := w.RunOnce(context.Background())
	if !ok || err == nil || store.failed != "SOURCE_ARCHIVE_INVALID" || agent.calls != 0 {
		t.Fatalf("changed source: ok=%v err=%v failed=%s calls=%d", ok, err, store.failed, agent.calls)
	}
}

func TestWorkerLeavesQueueUntouchedWhenAgentUnavailable(t *testing.T) {
	w, store, agent := testWorker(t)
	agent.readyErr = errors.New("connection refused")
	ok, err := w.RunOnce(context.Background())
	if ok || err == nil || store.claims != 0 || store.failed != "" || agent.calls != 0 {
		t.Fatalf("unavailable agent consumed task: ok=%v err=%v claims=%d", ok, err, store.claims)
	}
}

func TestWorkerPreparesPrivateProjectBeforeAgent(t *testing.T) {
	w, store, agent := testWorker(t)
	w.Verifier = fakeVerifier{projectErr: errors.New("Harbor unavailable")}
	ok, err := w.RunOnce(context.Background())
	if !ok || err == nil || agent.calls != 0 || store.completed ||
		store.failed != "BUILD_PROJECT_UNAVAILABLE" || !store.retry {
		t.Fatalf("project failure admitted a build: ok=%v err=%v calls=%d failed=%s retry=%v", ok, err, agent.calls, store.failed, store.retry)
	}
}
