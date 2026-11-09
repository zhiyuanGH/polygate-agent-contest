package deploy

import (
	"context"
	"errors"
	"testing"
	"time"

	"polygate/internal/domain"
)

type testStore struct {
	task      Task
	claimed   bool
	completed bool
	stopped   bool
	retryCode string
}

func (s *testStore) ClaimNext(context.Context, string, time.Duration) (Task, bool, error) {
	if s.claimed {
		return Task{}, false, nil
	}
	s.claimed = true
	return s.task, true, nil
}
func (s *testStore) Retry(_ context.Context, _, _, code string, _ time.Duration) error {
	s.retryCode = code
	return nil
}
func (s *testStore) Complete(context.Context, string, string) error {
	s.completed = true
	return nil
}
func (s *testStore) CompleteStop(context.Context, string, string) error {
	s.stopped = true
	return nil
}

type testCluster struct {
	observation Observation
	err         error
	ensured     bool
	deleted     bool
	gone        bool
}

func (c *testCluster) EnsureTenant(context.Context, string, domain.Quota) error { return nil }
func (c *testCluster) EnsureWorkload(context.Context, Task) error {
	c.ensured = true
	return c.err
}
func (c *testCluster) ObserveWorkload(context.Context, Task) (Observation, error) {
	return c.observation, nil
}
func (c *testCluster) DeleteWorkload(context.Context, Task) error {
	c.deleted = true
	return c.err
}
func (c *testCluster) WorkloadGone(context.Context, Task) (bool, error) {
	return c.gone, nil
}

func TestWorkerRequiresObservedReadyGenerationAndDigest(t *testing.T) {
	task := Task{ID: "task", DesiredState: "Running", ImageDigest: "approved@sha256:digest",
		Spec: domain.AppSpec{Resources: domain.Resources{Replicas: 2}}}
	for _, tc := range []struct {
		name  string
		state Observation
		ready bool
	}{
		{"stale generation", Observation{Generation: 2, ObservedGeneration: 1, DesiredReplicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, ImageDigest: task.ImageDigest}, false},
		{"one replica ready", Observation{Generation: 2, ObservedGeneration: 2, DesiredReplicas: 2, UpdatedReplicas: 2, ReadyReplicas: 1, ImageDigest: task.ImageDigest}, false},
		{"old replicas still ready", Observation{Generation: 2, ObservedGeneration: 2, DesiredReplicas: 2, UpdatedReplicas: 1, ReadyReplicas: 2, ImageDigest: task.ImageDigest}, false},
		{"old image ready", Observation{Generation: 2, ObservedGeneration: 2, DesiredReplicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, ImageDigest: "old-image"}, false},
		{"wrong replica target", Observation{Generation: 2, ObservedGeneration: 2, DesiredReplicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, ImageDigest: task.ImageDigest}, false},
		{"current revision ready", Observation{Generation: 2, ObservedGeneration: 2, DesiredReplicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, ImageDigest: task.ImageDigest}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &testStore{task: task}
			cluster := &testCluster{observation: tc.state}
			worker := Worker{Store: store, Cluster: cluster, ID: "worker", Lease: time.Minute}
			processed, err := worker.RunOnce(context.Background())
			if err != nil || !processed || !cluster.ensured {
				t.Fatalf("worker failed to reconcile: processed=%v err=%v", processed, err)
			}
			if store.completed != tc.ready {
				t.Fatalf("completed=%v, want %v", store.completed, tc.ready)
			}
			if !tc.ready && store.retryCode != "WAITING_FOR_READY" {
				t.Fatalf("retry code=%q", store.retryCode)
			}
		})
	}
}

func TestWorkerRetriesClusterFailure(t *testing.T) {
	store := &testStore{task: Task{ID: "task", DesiredState: "Running"}}
	cluster := &testCluster{err: errors.New("api unavailable")}
	worker := Worker{Store: store, Cluster: cluster, ID: "worker", Lease: time.Minute}
	processed, err := worker.RunOnce(context.Background())
	if err == nil || !processed || store.completed || store.retryCode != "KUBERNETES_RECONCILE_ERROR" {
		t.Fatalf("cluster failure: processed=%v err=%v completed=%v retry=%s",
			processed, err, store.completed, store.retryCode)
	}
}

func TestUpdateWaitsForOldPodsToExitBeforeReleasingSurge(t *testing.T) {
	for _, kind := range []string{"Update", "Rollback"} {
		for _, tc := range []struct{ replicas, oldPods int64 }{{2, 1}, {1, 1}, {1, 0}} {
			store := &testStore{task: Task{ID: "task", Kind: kind, DesiredState: "Running", ImageDigest: "approved@sha256:digest",
				Spec: domain.AppSpec{Resources: domain.Resources{Replicas: 1}}}}
			cluster := &testCluster{observation: Observation{Generation: 2, ObservedGeneration: 2,
				DesiredReplicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, TotalReplicas: tc.replicas, PodCount: tc.replicas, OldPods: tc.oldPods,
				ImageDigest: store.task.ImageDigest}}
			worker := Worker{Store: store, Cluster: cluster, ID: "worker", Lease: time.Minute}
			if processed, err := worker.RunOnce(context.Background()); err != nil || !processed {
				t.Fatalf("kind=%s replicas=%d old=%d processed=%v err=%v", kind, tc.replicas, tc.oldPods, processed, err)
			}
			if store.completed != (tc.replicas == 1 && tc.oldPods == 0) {
				t.Fatalf("kind=%s replicas=%d old=%d completed=%v", kind, tc.replicas, tc.oldPods, store.completed)
			}
		}
	}
}

func TestWorkerWaitsForApprovedImagePullCredential(t *testing.T) {
	store := &testStore{task: Task{ID: "task", DesiredState: "Running"}}
	cluster := &testCluster{err: ErrPullCredentialPending}
	worker := Worker{Store: store, Cluster: cluster, ID: "worker", Lease: time.Minute}
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed || store.completed || store.retryCode != "WAITING_FOR_PULL_CREDENTIAL" {
		t.Fatalf("pull credential wait: processed=%v err=%v completed=%v retry=%s",
			processed, err, store.completed, store.retryCode)
	}
}

func TestWorkerDoesNotReleaseStopUntilWorkloadIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone bool
	}{
		{"pods still exist", false},
		{"all objects gone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &testStore{task: Task{ID: "stop-task", DesiredState: "Stopped"}}
			cluster := &testCluster{gone: tc.gone}
			worker := Worker{Store: store, Cluster: cluster, ID: "worker", Lease: time.Minute}
			processed, err := worker.RunOnce(context.Background())
			if err != nil || !processed || !cluster.deleted {
				t.Fatalf("stop reconciliation failed: processed=%v err=%v", processed, err)
			}
			if store.stopped != tc.gone {
				t.Fatalf("released reservation=%v, workload gone=%v", store.stopped, tc.gone)
			}
			if !tc.gone && store.retryCode != "WAITING_FOR_PODS_TO_EXIT" {
				t.Fatalf("retry code=%q", store.retryCode)
			}
		})
	}
}
