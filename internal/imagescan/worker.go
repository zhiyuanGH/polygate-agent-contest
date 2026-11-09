package imagescan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"polygate/internal/harbor"
	"polygate/internal/postgres"
	"polygate/internal/registry"
)

const Lease = 15 * time.Minute
const WorkTimeout = 13 * time.Minute

type TaskStore interface {
	ClaimNextImageScan(context.Context, string, time.Duration) (postgres.ImageScanTask, bool, error)
	RetryImageScan(context.Context, string, string, string, time.Duration) error
	FailImageScan(context.Context, string, string, string) error
	RecordLeasedImageScan(context.Context, string, string, string, string, harbor.Evidence) (string, bool, error)
}

type HarborClient interface {
	MirrorAccountImage(context.Context, string, string, registry.Image, *registry.Resolver) (string, error)
	ScanAndReport(context.Context, string, string, string) (harbor.Evidence, error)
}

type Worker struct {
	Store    TaskStore
	Harbor   HarborClient
	Resolver *registry.Resolver
	ID       string
}

func retryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 30 * time.Second
	}
	delay := 30 * time.Second
	for i := 1; i < attempt && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		return 30 * time.Minute
	}
	return delay
}

// RunOnce consumes one durable task. External error text may include signed
// blob URLs or registry credentials and is deliberately excluded from errors
// returned to the caller's log.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.Store == nil || w.Harbor == nil || w.Resolver == nil || w.ID == "" {
		return false, errors.New("image scan worker is not configured")
	}
	task, ok, err := w.Store.ClaimNextImageScan(ctx, w.ID, Lease)
	if err != nil || !ok {
		return false, err
	}
	source := registry.Image{PinnedReference: task.PinnedReference,
		PlatformDigest: task.PlatformDigest, RegistryHost: task.RegistryHost}
	reference, err := w.Harbor.MirrorAccountImage(ctx, task.AccountID,
		task.ArtifactID, source, w.Resolver)
	if err != nil {
		permanent := errors.Is(err, registry.ErrSourcePolicy) ||
			errors.Is(err, registry.ErrSourceChanged) ||
			errors.Is(err, registry.ErrImageTooLarge) ||
			errors.Is(err, harbor.ErrMirrorDigestMismatch)
		return true, w.finishFailure(task, "IMAGE_MIRROR_FAILED", permanent)
	}
	project, err := harbor.AccountProjectName(task.AccountID)
	if err != nil {
		return true, w.finishFailure(task, "IMAGE_ACCOUNT_INVALID", true)
	}
	evidence, err := w.Harbor.ScanAndReport(ctx, project, "agent", task.PlatformDigest)
	if err != nil {
		return true, w.finishFailure(task, "IMAGE_SCAN_UNAVAILABLE", false)
	}
	if _, _, err := w.Store.RecordLeasedImageScan(ctx, task.AccountID,
		task.ArtifactID, w.ID, reference, evidence); err != nil {
		return true, w.finishFailure(task, "IMAGE_SCAN_EVIDENCE_INVALID", false)
	}
	return true, nil
}

func (w *Worker) finishFailure(task postgres.ImageScanTask, code string, permanent bool) error {
	// A canceled work context should not prevent a best-effort lease release.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	if permanent {
		err = w.Store.FailImageScan(ctx, task.ArtifactID, w.ID, code)
	} else {
		err = w.Store.RetryImageScan(ctx, task.ArtifactID, w.ID, code, retryDelay(task.Attempt))
	}
	if err != nil {
		return fmt.Errorf("image scan task %s could not record %s: %T", task.ArtifactID, code, err)
	}
	return fmt.Errorf("image scan task %s: %s", task.ArtifactID, code)
}
