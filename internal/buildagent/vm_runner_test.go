package buildagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeVMHelper struct {
	called int
	badLog bool
}

type synchronizedLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (l *synchronizedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Write(p)
}
func (l *synchronizedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.data.String() }

func (h *fakeVMHelper) Run(_ context.Context, _ string, args ...string) (string, error) {
	h.called++
	flags := make(map[string]string)
	for i := 0; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	if flags["--slot"] != "0" || flags["--task-id"] != "11111111-1111-4111-8111-111111111111" ||
		flags["--account-id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" ||
		flags["--source"] != filepath.Join(flags["--work-dir"], "source.zip") {
		return "", errors.New("helper received unexpected fixed arguments")
	}
	if _, err := os.Stat(flags["--source"]); err != nil {
		return "", err
	}
	log := []byte("fixed build log\n")
	logHash := sha256.Sum256(log)
	if err := os.WriteFile(filepath.Join(flags["--work-dir"], "build.log"), log, 0600); err != nil {
		return "", err
	}
	result := Result{ImageDigest: "sha256:" + strings.Repeat("b", 64),
		LogSHA256: "sha256:" + hex.EncodeToString(logHash[:])}
	if h.badLog {
		result.LogSHA256 = "sha256:" + strings.Repeat("c", 64)
	}
	encoded, _ := json.Marshal(result)
	return "", os.WriteFile(flags["--result"], encoded, 0600)
}

func pinnedTestFile(t *testing.T, path string, body []byte, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func testVMRunner(t *testing.T, helper *fakeVMHelper) (VMRunner, Request) {
	t.Helper()
	root := t.TempDir()
	helpPath := filepath.Join(root, "fixed-helper")
	basePath := filepath.Join(root, "base.qcow2")
	helpDigest := pinnedTestFile(t, helpPath, []byte("trusted-helper-binary"), 0500)
	baseDigest := pinnedTestFile(t, basePath, []byte("trusted-readonly-base"), 0400)
	workRoot := filepath.Join(root, "work")
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	runner := VMRunner{Cleaner: LibvirtCleaner{VirshPath: "/usr/bin/virsh",
		WorkRoot: workRoot, Commands: &fakeVirsh{}}, HelperPath: helpPath,
		HelperSHA256: helpDigest, BaseImagePath: basePath,
		BaseImageSHA256: baseDigest, TrustedOwnerUID: uint32(os.Geteuid()), Commands: helper}
	sourceBytes := []byte("validated source ZIP bytes")
	sourceHash := sha256.Sum256(sourceBytes)
	request := Request{TaskID: "11111111-1111-4111-8111-111111111111", Attempt: 1,
		AccountID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		SourceSHA256: "sha256:" + hex.EncodeToString(sourceHash[:]),
		SourceBytes:  int64(len(sourceBytes)), Source: bytes.NewReader(sourceBytes)}
	return runner, request
}

func TestVMRunnerStagesSourceAndCleansAfterVerifiedResult(t *testing.T) {
	helper := &fakeVMHelper{}
	runner, request := testVMRunner(t, helper)
	var streamed bytes.Buffer
	request.LogWriter = &streamed
	manager := NewManager(runner)
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), request)
	if err != nil || helper.called != 1 || !sha256Pattern.MatchString(result.ImageDigest) {
		t.Fatalf("VM execution: %+v %v calls=%d", result, err, helper.called)
	}
	if streamed.String() != "fixed build log\n" {
		t.Fatalf("streamed log: %q", streamed.String())
	}
	if _, err := os.Stat(filepath.Join(runner.Cleaner.WorkRoot, "slot-0")); !os.IsNotExist(err) {
		t.Fatalf("slot residue after success: %v", err)
	}
}

func TestVMRunnerRejectsShortSourceEvenWhenItsDigestMatches(t *testing.T) {
	helper := &fakeVMHelper{}
	runner, request := testVMRunner(t, helper)
	request.SourceBytes++
	manager := NewManager(runner)
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "source length") || helper.called != 0 {
		t.Fatalf("short source accepted: err=%v helper calls=%d", err, helper.called)
	}
	if _, err := os.Stat(filepath.Join(runner.Cleaner.WorkRoot, "slot-0")); !os.IsNotExist(err) {
		t.Fatalf("slot residue after rejected source: %v", err)
	}
}

func TestVMRunnerRejectsTamperedHelperAndLog(t *testing.T) {
	helper := &fakeVMHelper{}
	runner, request := testVMRunner(t, helper)
	if err := os.Chmod(runner.HelperPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner.HelperPath, []byte("changed"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runner.HelperPath, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Execute(context.Background(), 0, request); err == nil || helper.called != 0 {
		t.Fatalf("tampered helper executed: err=%v calls=%d", err, helper.called)
	}
	helper = &fakeVMHelper{badLog: true}
	runner, request = testVMRunner(t, helper)
	manager := NewManager(runner)
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), request); err == nil || helper.called != 1 {
		t.Fatalf("invalid build log accepted: err=%v calls=%d", err, helper.called)
	}
	if _, err := os.Stat(filepath.Join(runner.Cleaner.WorkRoot, "slot-0")); !os.IsNotExist(err) {
		t.Fatalf("slot residue after failure: %v", err)
	}
}

func TestVMRunnerRejectsWritableHelperParent(t *testing.T) {
	helper := &fakeVMHelper{}
	runner, request := testVMRunner(t, helper)
	parent := filepath.Join(filepath.Dir(runner.HelperPath), "replaceable")
	if err := os.Mkdir(parent, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(parent, "fixed-helper")
	runner.HelperSHA256 = pinnedTestFile(t, helpPath, []byte("trusted-helper-binary"), 0500)
	runner.HelperPath = helpPath
	if _, err := runner.Execute(context.Background(), 0, request); err == nil || helper.called != 0 {
		t.Fatalf("replaceable helper executed: err=%v calls=%d", err, helper.called)
	}
}

func TestFollowVMLogStreamsBeforeBuildFinishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	var output synchronizedLog
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- followVMLog(context.Background(), path, &output, done) }()
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for output.String() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if output.String() != "first\n" {
		t.Fatalf("live output: %q", output.String())
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("last\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	close(done)
	if err = <-finished; err != nil || output.String() != "first\nlast\n" {
		t.Fatalf("final output: %q %v", output.String(), err)
	}
}
