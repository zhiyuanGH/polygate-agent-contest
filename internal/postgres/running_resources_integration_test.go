package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestRunningResourceUpdateKeepsBudgetAndRestoresOnFailure(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accountID, _ := server.NewID()
	otherID, _ := server.NewID()
	for _, id := range []string{accountID, otherID} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
			VALUES($1,$2,$2,'Resource rollout test')`, id, "resource-rollout-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		tx, err := store.Pool.Begin(clean)
		if err != nil {
			return
		}
		defer tx.Rollback(clean)
		ids := []string{accountID, otherID}
		_, _ = tx.Exec(clean, `UPDATE global_budget SET
			reserved_cpu_request_milli=reserved_cpu_request_milli-(SELECT COALESCE(SUM(cpu_request_milli),0) FROM (SELECT cpu_request_milli FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT cpu_request_milli FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_cpu_limit_milli=reserved_cpu_limit_milli-(SELECT COALESCE(SUM(cpu_limit_milli),0) FROM (SELECT cpu_limit_milli FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT cpu_limit_milli FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_memory_mib=reserved_memory_mib-(SELECT COALESCE(SUM(memory_mib),0) FROM (SELECT memory_mib FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT memory_mib FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_pods=reserved_pods-(SELECT COALESCE(SUM(pods),0) FROM (SELECT pods FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT pods FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u)
			WHERE id=true`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM outbox WHERE task_id IN (SELECT id FROM deployment_tasks WHERE account_id=ANY($1::uuid[]))`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM rollout_reservations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM deployment_tasks WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM resource_reservations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM app_revisions WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM student_volumes WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, ids)
		_ = tx.Commit(clean)
	}()
	base := domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}
	app, _, err := store.Create(ctx, accountID, "owner", "resource-app-create", domain.AppSpec{
		Name: "resource-rollout", Image: "example.com/agent:v1", Port: 8080, Resources: base})
	if err != nil {
		t.Fatal(err)
	}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	initial, _, err := store.QueueTrustedDeploy(ctx, accountID, "owner", app.ID, "resource-initial-deploy", digest, 5678)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(worker, id string) {
		t.Helper()
		got, found, err := store.ClaimNext(ctx, worker, time.Minute)
		if err != nil || !found || got.ID != id {
			t.Fatalf("claim %s: %+v %v %v", id, got, found, err)
		}
	}
	claim("resource-initial-worker", initial.ID)
	if err := store.Complete(ctx, initial.ID, "resource-initial-worker"); err != nil {
		t.Fatal(err)
	}
	var budget, reserved domain.Usage
	readBudget := func() domain.Usage {
		t.Helper()
		var value domain.Usage
		if err := store.Pool.QueryRow(ctx, `SELECT reserved_cpu_request_milli,reserved_cpu_limit_milli,
			reserved_memory_mib,reserved_pods FROM global_budget WHERE id=true`).Scan(
			&value.CPURequestMilli, &value.CPULimitMilli, &value.MemoryMiB, &value.Pods); err != nil {
			t.Fatal(err)
		}
		return value
	}
	readReservation := func() domain.Usage {
		t.Helper()
		var value domain.Usage
		if err := store.Pool.QueryRow(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,pods
			FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(
			&value.CPURequestMilli, &value.CPULimitMilli, &value.MemoryMiB, &value.Pods); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if _, _, err := store.QueueRunningRestart(ctx, otherID, "other", app.ID,
		"restart-foreign-001", "Recreate"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account restart: %v", err)
	}
	restart, replay, err := store.QueueRunningRestart(ctx, accountID, "owner", app.ID,
		"restart-recreate-001", "Recreate")
	if err != nil || replay {
		t.Fatalf("queue restart: %+v %v %v", restart, replay, err)
	}
	if again, replay, err := store.QueueRunningRestart(ctx, accountID, "owner", app.ID,
		"restart-recreate-001", "Recreate"); err != nil || !replay || again.ID != restart.ID {
		t.Fatalf("restart replay: %+v %v %v", again, replay, err)
	}
	if _, _, err := store.QueueRunningRestart(ctx, accountID, "owner", app.ID,
		"restart-recreate-001", "Rolling"); domain.AsDomainError(err) == nil {
		t.Fatalf("reused restart key changed strategy: %v", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 512 || budget.Pods != 1 {
		t.Fatalf("Recreate restart changed budget: %+v", budget)
	}
	restartClaim, found, err := store.ClaimNext(ctx, "restart-worker", time.Minute)
	if err != nil || !found || restartClaim.ID != restart.ID || restartClaim.UpdateKind != "Restart" ||
		restartClaim.Spec.Resources != base {
		t.Fatalf("restart claim: %+v %v %v", restartClaim, found, err)
	}
	if err := store.Complete(ctx, restart.ID, "restart-worker"); err != nil {
		t.Fatal("complete restart:", err)
	}
	var restartState string
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, restart.OperationID).
		Scan(&restartState); err != nil || restartState != "Succeeded" {
		t.Fatalf("restart audit: %s %v", restartState, err)
	}
	if budget = readBudget(); budget.MemoryMiB != 512 || budget.Pods != 1 {
		t.Fatalf("completed restart changed budget: %+v", budget)
	}
	failedRestart, _, err := store.QueueRunningRestart(ctx, accountID, "owner", app.ID,
		"restart-recreate-fail", "Recreate")
	if err != nil {
		t.Fatal("queue failed restart:", err)
	}
	claim("restart-fail-worker", failedRestart.ID)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, failedRestart.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, failedRestart.ID, "restart-fail-worker", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("queue restart rollback:", err)
	}
	restartRollback, found, err := store.ClaimNext(ctx, "restart-rollback-worker", time.Minute)
	if err != nil || !found || restartRollback.Kind != "Rollback" ||
		restartRollback.UpdateKind != "Restart" || restartRollback.RevisionID != restart.RevisionID {
		t.Fatalf("restart rollback claim: %+v %v %v", restartRollback, found, err)
	}
	if err := store.Complete(ctx, restartRollback.ID, "restart-rollback-worker"); err != nil {
		t.Fatal("complete restart rollback:", err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, failedRestart.OperationID).
		Scan(&restartState); err != nil || restartState != "Failed" {
		t.Fatalf("failed restart audit: %s %v", restartState, err)
	}
	if budget = readBudget(); budget.MemoryMiB != 512 || budget.Pods != 1 {
		t.Fatalf("restart rollback changed budget: %+v", budget)
	}
	rolling := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 2}
	if _, _, err := store.QueueRunningResourceUpdate(ctx, otherID, "other", app.ID,
		"resource-foreign-update", rolling, "Rolling"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account update: %v", err)
	}
	update, replay, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-rolling-001", rolling, "Rolling")
	if err != nil || replay {
		t.Fatalf("queue rolling: %+v %v %v", update, replay, err)
	}
	if again, replay, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-rolling-001", rolling, "Rolling"); err != nil || !replay || again.ID != update.ID {
		t.Fatalf("rolling replay: %+v %v %v", again, replay, err)
	}
	if _, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-rolling-001", base, "Rolling"); domain.AsDomainError(err) == nil {
		t.Fatalf("reused key changed resources: %v", err)
	}
	budget = readBudget()
	if budget.MemoryMiB != 512+2048 || budget.Pods != 3 {
		t.Fatalf("rolling peak budget: %+v", budget)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT memory_mib,pods FROM rollout_reservations WHERE update_task_id=$1`,
		update.ID).Scan(&reserved.MemoryMiB, &reserved.Pods); err != nil || reserved.MemoryMiB != 2048 || reserved.Pods != 2 {
		t.Fatalf("rolling surge: %+v %v", reserved, err)
	}
	claimed, found, err := store.ClaimNext(ctx, "resource-rolling-worker", time.Minute)
	if err != nil || !found || claimed.ID != update.ID || claimed.Strategy != "Rolling" || claimed.Spec.Resources != rolling || claimed.Spec.Port != 5678 {
		t.Fatalf("rolling claim: %+v %v %v", claimed, found, err)
	}
	if err := store.Complete(ctx, update.ID, "resource-rolling-worker"); err != nil {
		t.Fatal("complete rolling resource change:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 2048 || budget.Pods != 2 {
		t.Fatalf("rolling final budget: %+v", budget)
	}
	if reserved = readReservation(); reserved.MemoryMiB != 2048 || reserved.Pods != 2 {
		t.Fatalf("rolling final reservation: %+v", reserved)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Spec.Resources != rolling || current.Status != "Running" {
		t.Fatalf("rolling app settings: %+v %v", current, err)
	}
	rollingFailureTarget := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 2048, Replicas: 1}
	rollingFailed, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-rolling-fail", rollingFailureTarget, "Rolling")
	if err != nil {
		t.Fatal("queue rolling failure:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 4096 || budget.Pods != 3 {
		t.Fatalf("rolling failure peak budget: %+v", budget)
	}
	claim("resource-rolling-fail-worker", rollingFailed.ID)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, rollingFailed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, rollingFailed.ID, "resource-rolling-fail-worker", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("queue rolling automatic rollback:", err)
	}
	rollingRollback, found, err := store.ClaimNext(ctx, "resource-rolling-rollback-worker", time.Minute)
	if err != nil || !found || rollingRollback.Kind != "Rollback" || rollingRollback.Strategy != "Rolling" || rollingRollback.RevisionID != update.RevisionID {
		t.Fatalf("rolling rollback claim: %+v %v %v", rollingRollback, found, err)
	}
	if err := store.Complete(ctx, rollingRollback.ID, "resource-rolling-rollback-worker"); err != nil {
		t.Fatal("complete rolling rollback:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 2048 || budget.Pods != 2 {
		t.Fatalf("rolling rollback budget: %+v", budget)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Spec.Resources != rolling || current.Status != "Running" {
		t.Fatalf("rolling rollback app settings: %+v %v", current, err)
	}
	down := base
	downUpdate, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-recreate-down", down, "Recreate")
	if err != nil {
		t.Fatal("queue Recreate decrease:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 2048 || budget.Pods != 2 {
		t.Fatalf("Recreate decrease changed budget early: %+v", budget)
	}
	claim("resource-down-worker", downUpdate.ID)
	if err := store.Complete(ctx, downUpdate.ID, "resource-down-worker"); err != nil {
		t.Fatal("complete Recreate decrease:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 512 || budget.Pods != 1 {
		t.Fatalf("Recreate decrease final budget: %+v", budget)
	}
	up := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 2048, Replicas: 1}
	failed, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", app.ID,
		"resource-recreate-fail", up, "Recreate")
	if err != nil {
		t.Fatal("queue Recreate increase:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 2048 || budget.Pods != 1 {
		t.Fatalf("Recreate increase budget: %+v", budget)
	}
	if reserved = readReservation(); reserved.MemoryMiB != 2048 || reserved.Pods != 1 {
		t.Fatalf("Recreate peak reservation: %+v", reserved)
	}
	claim("resource-fail-worker", failed.ID)
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, failed.ID, "resource-fail-worker", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("queue Recreate automatic rollback:", err)
	}
	rollback, found, err := store.ClaimNext(ctx, "resource-rollback-worker", time.Minute)
	if err != nil || !found || rollback.Kind != "Rollback" || rollback.Strategy != "Recreate" || rollback.RevisionID != downUpdate.RevisionID {
		t.Fatalf("Recreate rollback claim: %+v %v %v", rollback, found, err)
	}
	if err := store.Complete(ctx, rollback.ID, "resource-rollback-worker"); err != nil {
		t.Fatal("complete Recreate rollback:", err)
	}
	if budget = readBudget(); budget.MemoryMiB != 512 || budget.Pods != 1 {
		t.Fatalf("Recreate rollback budget: %+v", budget)
	}
	if reserved = readReservation(); reserved.MemoryMiB != 512 || reserved.Pods != 1 {
		t.Fatalf("Recreate rollback reservation: %+v", reserved)
	}
	if current, err := store.Get(ctx, accountID, app.ID); err != nil || current.Spec.Resources != down || current.Status != "Running" {
		t.Fatalf("Recreate rollback app settings: %+v %v", current, err)
	}
	var operationState string
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, failed.OperationID).Scan(&operationState); err != nil || operationState != "Failed" {
		t.Fatalf("resource failure audit: %s %v", operationState, err)
	}
	stop, _, err := store.QueueStop(ctx, accountID, "owner", app.ID, "resource-stop-001")
	if err != nil {
		t.Fatal(err)
	}
	claim("resource-stop-worker", stop.ID)
	if err := store.CompleteStop(ctx, stop.ID, "resource-stop-worker"); err != nil {
		t.Fatal(err)
	}
	if budget = readBudget(); budget.MemoryMiB != 0 || budget.Pods != 0 {
		t.Fatalf("resource stop did not release budget: %+v", budget)
	}
	if err := store.Delete(ctx, accountID, "owner", app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=true WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	volume, _, err := store.CreateVolume(ctx, accountID, "owner", "resource-volume-create",
		domain.VolumeSpec{Name: "resource-volume", SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE student_volumes SET state='Ready' WHERE id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	persistentSpec := domain.AppSpec{Name: "resource-persistent", Image: "example.com/agent:v1",
		Port: 5678, Resources: base, Persistent: true, VolumeID: volume.ID, MountPath: "/data"}
	persistentApp, _, err := store.Create(ctx, accountID, "owner", "resource-persistent-create", persistentSpec)
	if err != nil {
		t.Fatal(err)
	}
	persistentDeploy, _, err := store.QueueTrustedDeploy(ctx, accountID, "owner", persistentApp.ID,
		"resource-persistent-deploy", digest, 5678)
	if err != nil {
		t.Fatal(err)
	}
	claim("resource-persistent-deploy-worker", persistentDeploy.ID)
	if err := store.Complete(ctx, persistentDeploy.ID, "resource-persistent-deploy-worker"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", persistentApp.ID,
		"resource-persistent-rolling", down, "Rolling"); domain.AsDomainError(err) == nil {
		t.Fatalf("persistent Rolling was accepted: %v", err)
	}
	persistentUp := domain.Resources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}
	persistentUpdate, _, err := store.QueueRunningResourceUpdate(ctx, accountID, "owner", persistentApp.ID,
		"resource-persistent-recreate", persistentUp, "Recreate")
	if err != nil {
		t.Fatal(err)
	}
	claim("resource-persistent-update-worker", persistentUpdate.ID)
	if err := store.Complete(ctx, persistentUpdate.ID, "resource-persistent-update-worker"); err != nil {
		t.Fatal(err)
	}
	if budget = readBudget(); budget.MemoryMiB != 1024 || budget.Pods != 1 {
		t.Fatalf("persistent resource budget: %+v", budget)
	}
	if current, err := store.Get(ctx, accountID, persistentApp.ID); err != nil || current.Spec.Resources != persistentUp || current.Status != "Running" {
		t.Fatalf("persistent resource app settings: %+v %v", current, err)
	}
	persistentStop, _, err := store.QueueStop(ctx, accountID, "owner", persistentApp.ID, "resource-persistent-stop")
	if err != nil {
		t.Fatal(err)
	}
	claim("resource-persistent-stop-worker", persistentStop.ID)
	if err := store.CompleteStop(ctx, persistentStop.ID, "resource-persistent-stop-worker"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, accountID, "owner", persistentApp.ID); err != nil {
		t.Fatal(err)
	}
}
