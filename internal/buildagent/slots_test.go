package buildagent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu              sync.Mutex
	started         chan int
	release         chan struct{}
	cleanupFailures map[int]bool
	cleaned         []int
}

func (r *fakeRunner) StopAndClean(_ context.Context, slot int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleaned = append(r.cleaned, slot)
	if r.cleanupFailures[slot] {
		return errors.New("guest destroy failed")
	}
	return nil
}
func (r *fakeRunner) Execute(ctx context.Context, slot int, _ Request) (Result, error) {
	if r.started != nil {
		r.started <- slot
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	return Result{ImageDigest: "sha256:" + strings.Repeat("a", 64)}, nil
}
func testRequest(id string) Request {
	return Request{TaskID: id, Attempt: 1,
		AccountID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SourceSHA256: "sha256:" + strings.Repeat("b", 64),
		SourceBytes: 1, Source: bytes.NewReader([]byte("x"))}
}

func TestSlotsRequireRecoveryAndPhysicalCleanupBeforeReuse(t *testing.T) {
	r := &fakeRunner{started: make(chan int, 2), release: make(chan struct{}), cleanupFailures: map[int]bool{}}
	m := NewManager(r)
	first := testRequest("11111111-1111-4111-8111-111111111111")
	if _, err := m.Run(context.Background(), first); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pre-recovery run: %v", err)
	}
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, id := range []string{first.TaskID, "22222222-2222-4222-8222-222222222222"} {
		go func(taskID string) { _, err := m.Run(context.Background(), testRequest(taskID)); results <- err }(id)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-r.started:
		case <-time.After(2 * time.Second):
			t.Fatal("build did not occupy slot")
		}
	}
	duplicate := first
	duplicate.Attempt = 2
	if _, err := m.Run(context.Background(), duplicate); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate task started: %v", err)
	}
	if _, err := m.Run(context.Background(), testRequest("33333333-3333-4333-8333-333333333333")); !errors.Is(err, ErrBusy) {
		t.Fatalf("third build: %v", err)
	}
	close(r.release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("build did not finish")
		}
	}
	ready, busy, quarantined := m.Status()
	if !ready || busy != 0 || quarantined != 0 {
		t.Fatalf("slots after cleanup: %v %d %d", ready, busy, quarantined)
	}
	r.mu.Lock()
	count := len(r.cleaned)
	r.mu.Unlock()
	if count != 4 {
		t.Fatalf("startup recovery plus two task cleanups = %d", count)
	}
}

func TestCleanupFailureQuarantinesSlotUntilRecovery(t *testing.T) {
	r := &fakeRunner{cleanupFailures: map[int]bool{}}
	m := NewManager(r)
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.cleanupFailures[0] = true
	r.mu.Unlock()
	if _, err := m.Run(context.Background(), testRequest("11111111-1111-4111-8111-111111111111")); err == nil {
		t.Fatal("cleanup failure reported success")
	}
	_, busy, quarantined := m.Status()
	if busy != 0 || quarantined != 1 {
		t.Fatalf("quarantine: %d %d", busy, quarantined)
	}
	retry := testRequest("11111111-1111-4111-8111-111111111111")
	retry.Attempt = 2
	if _, err := m.Run(context.Background(), retry); !errors.Is(err, ErrBusy) {
		t.Fatalf("quarantined task restarted: %v", err)
	}
	if _, err := m.Run(context.Background(), testRequest("22222222-2222-4222-8222-222222222222")); err != nil {
		t.Fatalf("other slot unavailable: %v", err)
	}
	if _, err := m.Run(context.Background(), testRequest("33333333-3333-4333-8333-333333333333")); err != nil {
		t.Fatalf("clean slot not reusable: %v", err)
	}
	r.mu.Lock()
	r.cleanupFailures[0] = false
	r.mu.Unlock()
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, quarantined = m.Status()
	if quarantined != 0 {
		t.Fatal("recovery did not clear quarantine")
	}
}

func TestCancelledBuildStillCleansGuest(t *testing.T) {
	r := &fakeRunner{release: make(chan struct{}), cleanupFailures: map[int]bool{}}
	m := NewManager(r)
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Run(ctx, testRequest("11111111-1111-4111-8111-111111111111")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	_, busy, quarantined := m.Status()
	if busy != 0 || quarantined != 0 {
		t.Fatalf("slot not cleaned after cancel: %d %d", busy, quarantined)
	}
}

func TestFailedStartupRecoveryRejectsBuilds(t *testing.T) {
	r := &fakeRunner{cleanupFailures: map[int]bool{1: true}}
	m := NewManager(r)
	if err := m.Recover(context.Background()); err == nil {
		t.Fatal("failed guest cleanup accepted")
	}
	if _, err := m.Run(context.Background(), testRequest("11111111-1111-4111-8111-111111111111")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("build accepted after failed recovery: %v", err)
	}
	r.mu.Lock()
	r.cleanupFailures[1] = false
	r.mu.Unlock()
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(context.Background(), testRequest("11111111-1111-4111-8111-111111111111")); err != nil {
		t.Fatal(err)
	}
}
