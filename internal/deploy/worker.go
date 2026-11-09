package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"polygate/internal/domain"
)

var ErrLeaseLost = errors.New("deployment task lease lost")
var ErrPullCredentialPending = errors.New("approved image pull credential is not ready")
var ErrVolumePending = errors.New("bound student volume is not ready")

type Task struct {
	ID            string
	Kind          string
	UpdateKind    string
	Strategy      string
	DesiredState  string
	AccountID     string
	Username      string
	AppID         string
	RevisionID    string
	Spec          domain.AppSpec
	ImageDigest   string
	VolumeSizeGiB int
	Quota         domain.Quota
	Attempt       int
}

type Observation struct {
	Generation         int64
	ObservedGeneration int64
	DesiredReplicas    int64
	UpdatedReplicas    int64
	ReadyReplicas      int64
	TotalReplicas      int64
	PodCount           int64
	OldPods            int64
	ImageDigest        string
}

func (o Observation) Ready() bool {
	return o.Generation > 0 && o.ObservedGeneration >= o.Generation &&
		o.DesiredReplicas > 0 && o.UpdatedReplicas >= o.DesiredReplicas &&
		o.ReadyReplicas >= o.DesiredReplicas
}

type Store interface {
	ClaimNext(context.Context, string, time.Duration) (Task, bool, error)
	Retry(context.Context, string, string, string, time.Duration) error
	Complete(context.Context, string, string) error
	CompleteStop(context.Context, string, string) error
}

type Cluster interface {
	EnsureTenant(context.Context, string, domain.Quota) error
	EnsureWorkload(context.Context, Task) error
	ObserveWorkload(context.Context, Task) (Observation, error)
	DeleteWorkload(context.Context, Task) error
	WorkloadGone(context.Context, Task) (bool, error)
}

type Worker struct {
	Store   Store
	Cluster Cluster
	ID      string
	Lease   time.Duration
}

// RunOnce applies platform-generated objects and marks an app Running only
// after Kubernetes reports the new generation and all desired replicas Ready.
// Every cluster write must be idempotent because an expired lease is replayed.
func (w Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.Store == nil || w.Cluster == nil || w.ID == "" || w.Lease < 30*time.Second {
		return false, errors.New("worker requires a store, cluster, ID and lease of at least 30 seconds")
	}
	task, found, err := w.Store.ClaimNext(ctx, w.ID, w.Lease)
	if err != nil || !found {
		return found, err
	}
	if task.DesiredState == "Stopped" {
		return w.stop(ctx, task)
	}
	if task.DesiredState != "Running" {
		return true, fmt.Errorf("task %s has unsupported desired state %q", task.ID, task.DesiredState)
	}
	if err = w.Cluster.EnsureTenant(ctx, task.AccountID, task.Quota); err == nil {
		err = w.Cluster.EnsureWorkload(ctx, task)
	}
	var observed Observation
	if err == nil {
		observed, err = w.Cluster.ObserveWorkload(ctx, task)
	}
	if err == nil && observed.Ready() &&
		observed.DesiredReplicas == task.Spec.Resources.Replicas &&
		(task.Kind != "Update" && task.Kind != "Rollback" ||
			(observed.TotalReplicas == observed.DesiredReplicas && observed.PodCount == observed.DesiredReplicas && observed.OldPods == 0)) &&
		observed.ImageDigest == task.ImageDigest {
		return true, w.Store.Complete(ctx, task.ID, w.ID)
	}
	code := "WAITING_FOR_READY"
	if errors.Is(err, ErrPullCredentialPending) {
		code = "WAITING_FOR_PULL_CREDENTIAL"
	} else if errors.Is(err, ErrVolumePending) {
		code = "WAITING_FOR_VOLUME"
	} else if err != nil {
		code = "KUBERNETES_RECONCILE_ERROR"
	}
	if retryErr := w.Store.Retry(ctx, task.ID, w.ID, code, retryDelay(task.Attempt)); retryErr != nil {
		return true, fmt.Errorf("task %s retry: %w", task.ID, retryErr)
	}
	if err != nil {
		if errors.Is(err, ErrPullCredentialPending) || errors.Is(err, ErrVolumePending) {
			return true, nil
		}
		return true, fmt.Errorf("task %s reconcile: %w", task.ID, err)
	}
	return true, nil
}

func (w Worker) stop(ctx context.Context, task Task) (bool, error) {
	err := w.Cluster.DeleteWorkload(ctx, task)
	var gone bool
	if err == nil {
		gone, err = w.Cluster.WorkloadGone(ctx, task)
	}
	if err == nil && gone {
		return true, w.Store.CompleteStop(ctx, task.ID, w.ID)
	}
	code := "WAITING_FOR_PODS_TO_EXIT"
	if err != nil {
		code = "KUBERNETES_DELETE_ERROR"
	}
	if retryErr := w.Store.Retry(ctx, task.ID, w.ID, code, retryDelay(task.Attempt)); retryErr != nil {
		return true, fmt.Errorf("stop task %s retry: %w", task.ID, retryErr)
	}
	if err != nil {
		return true, fmt.Errorf("stop task %s delete: %w", task.ID, err)
	}
	return true, nil
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		return 2 * time.Second
	}
	if attempt > 5 {
		attempt = 5
	}
	return time.Duration(1<<attempt) * time.Second
}
