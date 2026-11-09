package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"polygate/internal/deploy"
	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestQueueTrustedDeployIsAtomicAndIdempotent(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	accountID, _ := server.NewID()
	_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
        VALUES($1,$2,$2,'Deploy Test')`, accountID, "deploy-test-"+accountID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		tx, err := store.Pool.Begin(cleanupCtx)
		if err != nil {
			return
		}
		defer tx.Rollback(cleanupCtx)
		_, _ = tx.Exec(cleanupCtx, `UPDATE global_budget SET
            reserved_cpu_request_milli=reserved_cpu_request_milli-(SELECT COALESCE(SUM(cpu_request_milli),0) FROM resource_reservations WHERE account_id=$1),
            reserved_cpu_limit_milli=reserved_cpu_limit_milli-(SELECT COALESCE(SUM(cpu_limit_milli),0) FROM resource_reservations WHERE account_id=$1),
            reserved_memory_mib=reserved_memory_mib-(SELECT COALESCE(SUM(memory_mib),0) FROM resource_reservations WHERE account_id=$1),
            reserved_pods=reserved_pods-(SELECT COALESCE(SUM(pods),0) FROM resource_reservations WHERE account_id=$1)
            WHERE id=true`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM outbox WHERE task_id IN (SELECT id FROM deployment_tasks WHERE account_id=$1)`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM deployment_tasks WHERE account_id=$1`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM resource_reservations WHERE account_id=$1`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM app_revisions WHERE account_id=$1`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM operations WHERE account_id=$1`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM apps WHERE account_id=$1`, accountID)
		_, _ = tx.Exec(cleanupCtx, `DELETE FROM accounts WHERE id=$1`, accountID)
		_ = tx.Commit(cleanupCtx)
	})
	spec := domain.AppSpec{Name: "deploy-test", Image: "student.example/image:latest", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, accountID, "test-subject", "create-draft-001", spec)
	if err != nil {
		t.Fatal(err)
	}
	var before domain.Usage
	err = store.Pool.QueryRow(ctx, `SELECT reserved_cpu_request_milli,reserved_cpu_limit_milli,
        reserved_memory_mib,reserved_pods FROM global_budget WHERE id=true`).Scan(
		&before.CPURequestMilli, &before.CPULimitMilli, &before.MemoryMiB, &before.Pods)
	if err != nil {
		t.Fatal(err)
	}
	digest := "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan DeploymentTask, attempts)
	errorsFound := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-request-001", digest, 5678)
			results <- task
			errorsFound <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent retry failed: %v", err)
		}
	}
	var taskID, operationID, revisionID string
	for task := range results {
		if taskID == "" {
			taskID = task.ID
			operationID = task.OperationID
			revisionID = task.RevisionID
		} else if task.ID != taskID {
			t.Fatalf("duplicate request created task %s and %s", taskID, task.ID)
		}
		if task.OperationID == "" || task.OperationID != operationID {
			t.Fatalf("duplicate request changed operation ID: %+v", task)
		}
	}
	if taskID == "" {
		t.Fatal("no task created")
	}
	if _, replay, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-request-001", digest, 5678); err != nil || !replay {
		t.Fatalf("idempotent replay: replay=%v err=%v", replay, err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-request-001", digest[:len(digest)-1]+"b", 5678); domain.AsDomainError(err) == nil {
		t.Fatalf("reused key with changed image digest accepted: %v", err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-request-001", digest, 8080); domain.AsDomainError(err) == nil {
		t.Fatalf("reused key with changed platform port accepted: %v", err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-request-002", digest, 5678); domain.AsDomainError(err) == nil {
		t.Fatalf("second deployment request accepted while queued: %v", err)
	}
	var taskCount, reservationCount, revisionCount, outboxCount, operationCount int
	err = store.Pool.QueryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM deployment_tasks WHERE account_id=$1),
        (SELECT COUNT(*) FROM resource_reservations WHERE account_id=$1),
        (SELECT COUNT(*) FROM app_revisions WHERE account_id=$1),
        (SELECT COUNT(*) FROM outbox o JOIN deployment_tasks t ON t.id=o.task_id WHERE t.account_id=$1),
        (SELECT COUNT(*) FROM operations WHERE account_id=$1 AND kind='DEPLOY_REQUEST')`, accountID).Scan(
		&taskCount, &reservationCount, &revisionCount, &outboxCount, &operationCount)
	if err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || reservationCount != 1 || revisionCount != 1 || outboxCount != 1 || operationCount != 1 {
		t.Fatalf("non-atomic persistence: tasks=%d reservations=%d revisions=%d outbox=%d operations=%d",
			taskCount, reservationCount, revisionCount, outboxCount, operationCount)
	}
	page, err := store.ListRevisions(ctx, accountID, app.ID, 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != revisionID ||
		page.Items[0].ImageDigest != digest || page.Items[0].SourceKind != "trusted" ||
		page.Items[0].Spec.Name != spec.Name || page.NextBeforeRevision != 0 {
		t.Fatalf("account revision history: %+v, %v", page, err)
	}
	otherAccount, _ := server.NewID()
	if _, err := store.ListRevisions(ctx, otherAccount, app.ID, 0, 50); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("another account could list revision history: %v", err)
	}
	if _, err := store.ListRevisions(ctx, accountID, "malformed-id", 0, 50); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("malformed app ID did not fail closed: %v", err)
	}
	var after domain.Usage
	err = store.Pool.QueryRow(ctx, `SELECT reserved_cpu_request_milli,reserved_cpu_limit_milli,
        reserved_memory_mib,reserved_pods FROM global_budget WHERE id=true`).Scan(
		&after.CPURequestMilli, &after.CPULimitMilli, &after.MemoryMiB, &after.Pods)
	if err != nil {
		t.Fatal(err)
	}
	if after.Sub(before) != spec.Resources.Usage().Sub(domain.Usage{Apps: 1}) {
		t.Fatalf("global budget changed by %+v, want %+v", after.Sub(before), spec.Resources.Usage())
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Queued" {
		t.Fatalf("app status = %+v, %v", current, err)
	}
	if status, err := store.DeploymentStatus(ctx, accountID, app.ID); err != nil ||
		status.Status != "Queued" || status.TaskID != taskID || status.TaskState != "Queued" {
		t.Fatalf("queued status = %+v, %v", status, err)
	}
	first, found, err := store.ClaimNext(ctx, "worker-a", 30*time.Second)
	if err != nil || !found || first.ID != taskID || first.Attempt != 1 {
		t.Fatalf("first claim: found=%v task=%+v err=%v", found, first, err)
	}
	if first.Spec.Port != 5678 || first.Spec.Image != spec.Image {
		t.Fatalf("trusted sample revision did not replace the draft port: %+v", first.Spec)
	}
	if _, found, err := store.ClaimNext(ctx, "worker-b", 30*time.Second); err != nil || found {
		t.Fatalf("active lease was claimed twice: found=%v err=%v", found, err)
	}
	if err := store.Retry(ctx, taskID, "worker-a", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("retry task:", err)
	}
	second, found, err := store.ClaimNext(ctx, "worker-b", 30*time.Second)
	if err != nil || !found || second.Attempt != 2 {
		t.Fatalf("second claim: found=%v task=%+v err=%v", found, second, err)
	}
	if err := store.Complete(ctx, taskID, "worker-a"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("stale worker could complete task: %v", err)
	}
	_, err = store.Pool.Exec(ctx, `UPDATE deployment_tasks SET lease_until=now()-interval '1 second' WHERE id=$1`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	third, found, err := store.ClaimNext(ctx, "worker-c", 30*time.Second)
	if err != nil || !found || third.Attempt != 3 {
		t.Fatalf("expired lease was not recovered: found=%v task=%+v err=%v", found, third, err)
	}
	if err := store.Complete(ctx, taskID, "worker-b"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("expired worker could complete task: %v", err)
	}
	if err := store.Complete(ctx, taskID, "worker-c"); err != nil {
		t.Fatal("complete recovered task:", err)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Running" {
		t.Fatalf("completed app status = %+v, %v", current, err)
	}
	if status, err := store.DeploymentStatus(ctx, accountID, app.ID); err != nil ||
		status.Status != "Running" || status.TaskState != "Succeeded" || status.Attempt != 3 {
		t.Fatalf("running status = %+v, %v", status, err)
	}
	var taskState, outboxState, auditState string
	err = store.Pool.QueryRow(ctx, `SELECT t.state,o.state,p.state
        FROM deployment_tasks t JOIN outbox o ON o.task_id=t.id
        JOIN operations p ON p.account_id=t.account_id AND p.app_id=t.app_id
            AND p.kind='DEPLOY_REQUEST' AND p.detail->>'taskId'=t.id::text
        WHERE t.id=$1`, taskID).Scan(&taskState, &outboxState, &auditState)
	if err != nil || taskState != "Succeeded" || outboxState != "Dispatched" || auditState != "Succeeded" {
		t.Fatalf("completion state: task=%s outbox=%s audit=%s err=%v", taskState, outboxState, auditState, err)
	}
	stop, replay, err := store.QueueStop(ctx, accountID, "test-subject", app.ID, "stop-request-001")
	if err != nil || replay || stop.DesiredState != "Stopped" || stop.OperationID == "" {
		t.Fatalf("queue stop: task=%+v replay=%v err=%v", stop, replay, err)
	}
	if duplicate, replay, err := store.QueueStop(ctx, accountID, "test-subject", app.ID, "stop-request-001"); err != nil || !replay ||
		duplicate.ID != stop.ID || duplicate.OperationID != stop.OperationID {
		t.Fatalf("stop replay: task=%+v replay=%v err=%v", duplicate, replay, err)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Stopping" {
		t.Fatalf("queued stop status = %+v, %v", current, err)
	}
	if status, err := store.DeploymentStatus(ctx, accountID, app.ID); err != nil ||
		status.TaskID != stop.ID || status.TaskState != "Queued" {
		t.Fatalf("stop status = %+v, %v", status, err)
	}
	stopTask, found, err := store.ClaimNext(ctx, "worker-stop", 30*time.Second)
	if err != nil || !found || stopTask.ID != stop.ID || stopTask.DesiredState != "Stopped" {
		t.Fatalf("stop claim: found=%v task=%+v err=%v", found, stopTask, err)
	}
	if err := store.CompleteStop(ctx, stop.ID, "other-worker"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("stale worker completed stop: %v", err)
	}
	var reservedBeforeRelease int64
	if err := store.Pool.QueryRow(ctx, `SELECT reserved_memory_mib FROM global_budget WHERE id=true`).Scan(&reservedBeforeRelease); err != nil {
		t.Fatal(err)
	}
	if reservedBeforeRelease != before.MemoryMiB+spec.Resources.Usage().MemoryMiB {
		t.Fatalf("stop released budget before workload was gone: %d", reservedBeforeRelease)
	}
	if err := store.CompleteStop(ctx, stop.ID, "worker-stop"); err != nil {
		t.Fatal("complete stop:", err)
	}
	var reservedAfterRelease int64
	if err := store.Pool.QueryRow(ctx, `SELECT reserved_memory_mib FROM global_budget WHERE id=true`).Scan(&reservedAfterRelease); err != nil {
		t.Fatal(err)
	}
	if reservedAfterRelease != before.MemoryMiB {
		t.Fatalf("stop left a budget reservation: got %d want %d", reservedAfterRelease, before.MemoryMiB)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Stopped" {
		t.Fatalf("completed stop status = %+v, %v", current, err)
	}
	var remainingReservations int
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(&remainingReservations); err != nil || remainingReservations != 0 {
		t.Fatalf("stop left reservation: count=%d err=%v", remainingReservations, err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "", digest, 5678); err == nil || errors.Is(err, server.ErrUnavailable) {
		t.Fatal("invalid request key did not get a domain validation error")
	}
	// A student can cancel a leased deployment. The cleanup task remains
	// queued until the old lease has expired, and the stale worker cannot
	// commit a Running state after cancellation.
	cancelledDeploy, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-cancel-001", digest, 5678)
	if err != nil {
		t.Fatal("redeploy for cancellation:", err)
	}
	claimed, found, err := store.ClaimNext(ctx, "worker-cancel", 30*time.Second)
	if err != nil || !found || claimed.ID != cancelledDeploy.ID {
		t.Fatalf("claim pending deployment: %+v %v %v", claimed, found, err)
	}
	cancelStop, replay, err := store.QueueStop(ctx, accountID, "test-subject", app.ID, "cancel-stop-001")
	if err != nil || replay {
		t.Fatalf("cancel deployment: %+v %v %v", cancelStop, replay, err)
	}
	if _, found, err := store.ClaimNext(ctx, "worker-early", 30*time.Second); err != nil || found {
		t.Fatalf("cleanup ran before old lease expiry: found=%v err=%v", found, err)
	}
	if err := store.Complete(ctx, cancelledDeploy.ID, "worker-cancel"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("cancelled worker completed deployment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET lease_until=now()-interval '1 second' WHERE id=$1`, cancelStop.ID); err != nil {
		t.Fatal(err)
	}
	cancelCleanup, found, err := store.ClaimNext(ctx, "worker-cleanup", 30*time.Second)
	if err != nil || !found || cancelCleanup.ID != cancelStop.ID {
		t.Fatalf("claim cancellation cleanup: %+v %v %v", cancelCleanup, found, err)
	}
	if err := store.CompleteStop(ctx, cancelStop.ID, "worker-cleanup"); err != nil {
		t.Fatal("finish cancellation cleanup:", err)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Stopped" {
		t.Fatalf("cancelled app status = %+v, %v", current, err)
	}
	// An exhausted deployment must enqueue cleanup. The reservation stays
	// held until cleanup completes, then the app is visibly Failed and can
	// be deployed again.
	timedOut, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-timeout-001", digest, 5678)
	if err != nil {
		t.Fatal("redeploy for timeout:", err)
	}
	timedOutClaim, found, err := store.ClaimNext(ctx, "worker-timeout", 30*time.Second)
	if err != nil || !found || timedOutClaim.ID != timedOut.ID {
		t.Fatalf("claim timeout deployment: %+v %v %v", timedOutClaim, found, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, timedOut.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, timedOut.ID, "worker-timeout", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("timeout transition:", err)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Stopping" {
		t.Fatalf("failed deploy released without cleanup: %+v %v", current, err)
	}
	autoCleanup, found, err := store.ClaimNext(ctx, "worker-auto-cleanup", 30*time.Second)
	if err != nil || !found || autoCleanup.DesiredState != "Stopped" || autoCleanup.AppID != app.ID {
		t.Fatalf("claim automatic cleanup: %+v %v %v", autoCleanup, found, err)
	}
	if err := store.CompleteStop(ctx, autoCleanup.ID, "worker-auto-cleanup"); err != nil {
		t.Fatal("finish automatic cleanup:", err)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Status != "Failed" {
		t.Fatalf("failed app status = %+v, %v", current, err)
	}
	var finalMemory int64
	if err := store.Pool.QueryRow(ctx, `SELECT reserved_memory_mib FROM global_budget WHERE id=true`).Scan(&finalMemory); err != nil || finalMemory != before.MemoryMiB {
		t.Fatalf("cleanup budget leak: memory=%d baseline=%d err=%v", finalMemory, before.MemoryMiB, err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, accountID, "test-subject", app.ID, "deploy-after-failure", digest, 5678); err != nil {
		t.Fatal("cleaned-up failure cannot redeploy:", err)
	}
}
