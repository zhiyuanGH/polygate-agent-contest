package volume

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Task struct {
	ID        string
	AccountID string
	SizeGiB   int
	State     string
	PVCUID    string
	PVName    string
	Handle    string
}

type Store interface {
	ClaimNextVolume(context.Context, string, time.Duration) (Task, bool, error)
	RetryVolume(context.Context, string, string, time.Duration) error
	CompleteVolume(context.Context, string, string) error
	RecordDeletionTarget(context.Context, Task, string) error
	CompleteDeletion(context.Context, Task, string) error
}

type Cluster interface {
	EnsureClaim(context.Context, Task) (bool, error)
	DiscoverDeletionTarget(context.Context, Task) (Task, error)
	DestroyClaim(context.Context, Task) (bool, error)
}

type Worker struct {
	Store   Store
	Cluster Cluster
	ID      string
	Lease   time.Duration
}

func (w Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.Store == nil || w.Cluster == nil || w.ID == "" || w.Lease < 30*time.Second {
		return false, errors.New("volume worker requires a store, cluster, ID and lease of at least 30 seconds")
	}
	task, found, err := w.Store.ClaimNextVolume(ctx, w.ID, w.Lease)
	if err != nil || !found {
		return found, err
	}
	if task.State == "Deleting" {
		if task.PVCUID == "" {
			target, discoverErr := w.Cluster.DiscoverDeletionTarget(ctx, task)
			if discoverErr != nil {
				return true, w.retry(ctx, task.ID, discoverErr)
			}
			if err := w.Store.RecordDeletionTarget(ctx, target, w.ID); err != nil {
				return true, fmt.Errorf("volume %s record deletion target: %w", task.ID, err)
			}
			task = target
		}
		done, destroyErr := w.Cluster.DestroyClaim(ctx, task)
		if destroyErr != nil || !done {
			return true, w.retry(ctx, task.ID, destroyErr)
		}
		return true, w.Store.CompleteDeletion(ctx, task, w.ID)
	}
	ready, err := w.Cluster.EnsureClaim(ctx, task)
	if err == nil && ready {
		return true, w.Store.CompleteVolume(ctx, task.ID, w.ID)
	}
	return true, w.retry(ctx, task.ID, err)
}

func (w Worker) retry(ctx context.Context, volumeID string, cause error) error {
	if retryErr := w.Store.RetryVolume(ctx, volumeID, w.ID, 10*time.Second); retryErr != nil {
		return fmt.Errorf("volume %s retry: %w", volumeID, retryErr)
	}
	if cause != nil {
		return fmt.Errorf("volume %s reconcile: %w", volumeID, cause)
	}
	return nil
}
