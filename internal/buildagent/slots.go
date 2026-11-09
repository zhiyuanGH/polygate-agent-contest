package buildagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

const SlotCount = 2
const BuildTimeout = 15 * time.Minute
const CleanupTimeout = 2 * time.Minute

var taskIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var (
	ErrNotReady    = errors.New("build host has not completed guest recovery")
	ErrBusy        = errors.New("all build VM slots are occupied or quarantined")
	ErrInvalidTask = errors.New("invalid build task identity")
)

type Request struct {
	TaskID       string
	Attempt      int
	AccountID    string
	SourceSHA256 string
	SourceBytes  int64
	Source       io.ReaderAt
	LogWriter    io.Writer
}

type Result struct {
	ImageDigest string `json:"imageDigest"`
	LogSHA256   string `json:"logSha256"`
}

// Runner is implemented by the host-specific libvirt backend. StopAndClean
// must be idempotent and must remove the guest, overlay, seed, and credentials
// for a fixed slot. Execute must honor context cancellation before returning.
type Runner interface {
	StopAndClean(context.Context, int) error
	Execute(context.Context, int, Request) (Result, error)
}

type slot struct {
	busy        bool
	quarantined bool
	taskID      string
}

type Manager struct {
	mu     sync.Mutex
	runner Runner
	ready  bool
	slots  [SlotCount]slot
}

func NewManager(runner Runner) *Manager { return &Manager{runner: runner} }

// Recover must run before accepting tasks. It also permits retrying a slot
// quarantined by a failed cleanup, without restarting the service.
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner == nil {
		return ErrNotReady
	}
	for _, item := range m.slots {
		if item.busy {
			return ErrBusy
		}
	}
	m.ready = false
	var failures error
	for i := range m.slots {
		cleanupCtx, cancel := context.WithTimeout(ctx, CleanupTimeout)
		err := m.runner.StopAndClean(cleanupCtx, i)
		cancel()
		m.slots[i].quarantined = err != nil
		if err == nil {
			m.slots[i].taskID = ""
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("slot %d: %w", i, err))
		}
	}
	if failures != nil {
		return failures
	}
	m.ready = true
	return nil
}

// Run keeps the physical slot occupied through guest destruction. A clean
// build result is not returned if guest destruction fails.
func (m *Manager) Run(ctx context.Context, request Request) (Result, error) {
	if !taskIDPattern.MatchString(request.TaskID) || request.Attempt < 1 || request.Attempt > 3 ||
		!taskIDPattern.MatchString(request.AccountID) || !sha256Pattern.MatchString(request.SourceSHA256) ||
		request.SourceBytes < 1 || request.SourceBytes > 100<<20 || request.Source == nil {
		return Result{}, ErrInvalidTask
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	if !m.ready {
		m.mu.Unlock()
		return Result{}, ErrNotReady
	}
	selected := -1
	for _, item := range m.slots {
		if item.taskID == request.TaskID {
			m.mu.Unlock()
			return Result{}, ErrBusy
		}
	}
	for i, item := range m.slots {
		if !item.busy && !item.quarantined {
			selected = i
			m.slots[i].busy = true
			m.slots[i].taskID = request.TaskID
			break
		}
	}
	m.mu.Unlock()
	if selected < 0 {
		return Result{}, ErrBusy
	}
	workCtx, cancel := context.WithTimeout(ctx, BuildTimeout)
	result, runErr := m.runner.Execute(workCtx, selected, request)
	if deadlineErr := workCtx.Err(); deadlineErr != nil {
		runErr = errors.Join(runErr, deadlineErr)
	}
	cancel()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), CleanupTimeout)
	cleanupErr := m.runner.StopAndClean(cleanupCtx, selected)
	cleanupCancel()
	m.mu.Lock()
	m.slots[selected].busy = false
	m.slots[selected].quarantined = cleanupErr != nil
	if cleanupErr == nil {
		m.slots[selected].taskID = ""
	}
	m.mu.Unlock()
	if cleanupErr != nil {
		return Result{}, errors.Join(runErr, fmt.Errorf("slot %d cleanup: %w", selected, cleanupErr))
	}
	if runErr != nil {
		return Result{}, runErr
	}
	return result, nil
}

func (m *Manager) Status() (ready bool, busy int, quarantined int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.slots {
		if item.busy {
			busy++
		}
		if item.quarantined {
			quarantined++
		}
	}
	return m.ready, busy, quarantined
}
