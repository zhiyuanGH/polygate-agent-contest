package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/server"
)

func appStorageTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to an isolated migrated database")
	}
	store, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := server.NewID()
	other, _ := server.NewID()
	ids := []string{owner, other}
	t.Cleanup(func() {
		defer store.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, query := range []string{
			`DELETE FROM rollout_reservations WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM resource_reservations WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM outbox WHERE task_id IN (SELECT id FROM deployment_tasks WHERE account_id=ANY($1::uuid[]))`,
			`DELETE FROM deployment_tasks WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM app_revisions WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM operations WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM apps WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM student_volumes WHERE account_id=ANY($1::uuid[])`,
			`DELETE FROM accounts WHERE id=ANY($1::uuid[])`,
		} {
			if _, err := store.Pool.Exec(ctx, query, ids); err != nil {
				t.Errorf("storage test cleanup: %v", err)
			}
		}
	})
	for _, id := range ids {
		if _, err := store.Pool.Exec(t.Context(), `INSERT INTO accounts(id,oidc_subject,username,display_name,volume_enabled)
			VALUES($1,$2,$2,'App storage test',true)`, id, "storage-"+id); err != nil {
			t.Fatal(err)
		}
	}
	return store, owner, other
}

func appStorageTestApp(t *testing.T, store *Store, owner, name string) server.App {
	t.Helper()
	spec := domain.AppSpec{Name: name, Image: "example.com/app:v1", Port: 8000,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(t.Context(), owner, "owner", "create-"+name, spec)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func appStorageTestVolume(t *testing.T, store *Store, owner, name string, ready bool) server.Volume {
	t.Helper()
	volume, _, err := store.CreateVolume(t.Context(), owner, "owner", "volume-"+name, domain.VolumeSpec{Name: name, SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		if _, err := store.Pool.Exec(t.Context(), `UPDATE student_volumes SET state='Ready' WHERE id=$1`, volume.ID); err != nil {
			t.Fatal(err)
		}
		volume.State = "Ready"
	}
	return volume
}

func TestAppStorageEditIsScopedAtomicAndPreservesData(t *testing.T) {
	store, owner, other := appStorageTestStore(t)
	ctx := t.Context()
	app := appStorageTestApp(t, store, owner, "agent")
	volume := appStorageTestVolume(t, store, owner, "data", true)
	unready := appStorageTestVolume(t, store, owner, "pending-data", false)
	foreign := appStorageTestVolume(t, store, other, "foreign-data", true)
	_, beforeUsage, _ := store.List(ctx, owner)
	if _, err := store.UpdateAppStorage(ctx, other, "other", app.ID, true, foreign.ID, "/data"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account app changed: %v", err)
	}
	for _, id := range []string{foreign.ID, unready.ID} {
		if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, id, "/data"); code(err) != "VOLUME_NOT_READY" {
			t.Fatalf("unavailable volume %s accepted: %v", id, err)
		}
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/proc/data"); code(err) != "INVALID_MOUNT_PATH" {
		t.Fatalf("unsafe mount accepted: %v", err)
	}
	updated, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data")
	if err != nil || updated.ID != app.ID || updated.Status != "Draft" || !updated.Spec.Persistent || updated.Spec.VolumeID != volume.ID {
		t.Fatalf("attach existing app: %+v %v", updated, err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data"); err != nil {
		t.Fatalf("unchanged edit: %v", err)
	}
	second := appStorageTestApp(t, store, owner, "second")
	multiple := second.Spec.Resources
	multiple.Replicas = 2
	if _, err := store.UpdateAppResources(ctx, owner, "owner", second.ID, multiple); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", second.ID, true, volume.ID, "/data"); code(err) != "INVALID_REPLICAS" {
		t.Fatalf("multiple replicas accepted for persistent storage: %v", err)
	}
	if _, err := store.UpdateAppResources(ctx, owner, "owner", second.ID, second.Spec.Resources); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", second.ID, true, volume.ID, "/data"); code(err) != "VOLUME_ALREADY_BOUND" {
		t.Fatalf("already-bound volume accepted: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=false WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/saved"); !errors.Is(err, server.ErrVolumeDisabled) {
		t.Fatalf("disabled volume capability ignored: %v", err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, false, "", ""); err != nil {
		t.Fatalf("detach existing binding: %v", err)
	}
	if got, err := store.GetVolume(ctx, owner, volume.ID); err != nil || got != volume {
		t.Fatalf("detach mutated/deleted volume: %+v %v", got, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=true WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", second.ID, true, volume.ID, "/data"); err != nil {
		t.Fatalf("detached volume unavailable for reuse: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=false WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", second.ID, false, "", ""); !errors.Is(err, server.ErrDisabled) {
		t.Fatalf("disabled account changed storage: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=true WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, owner, app.ID)
	before, _ := json.Marshal(app.Spec)
	after, _ := json.Marshal(got.Spec)
	if err != nil || string(before) != string(after) {
		t.Fatalf("detach did not preserve other settings: %+v %v", got, err)
	}
	_, usage, _ := store.List(ctx, owner)
	if usage != beforeUsage.Add(second.Spec.Resources.Usage()) {
		t.Fatalf("storage edits changed configured capacity: %+v", usage)
	}
	ops, err := store.ListOperations(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, op := range ops {
		if op.Kind == "UPDATE_APP_STORAGE" {
			count++
			var detail map[string]any
			if json.Unmarshal(op.Detail, &detail) != nil || detail["before"] == nil || detail["after"] == nil {
				t.Fatalf("missing storage audit before/after: %s", op.Detail)
			}
		}
	}
	if count != 3 {
		t.Fatalf("expected attach, detach and reuse audits only; got %d", count)
	}
}

func TestAppStorageEditRequiresVerifiedCleanup(t *testing.T) {
	store, owner, _ := appStorageTestStore(t)
	ctx := t.Context()
	app := appStorageTestApp(t, store, owner, "agent")
	volume := appStorageTestVolume(t, store, owner, "data", true)
	setStatus := func(status string) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `UPDATE apps SET status=$2 WHERE id=$1`, app.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"Queued", "Deploying", "Running", "Stopping", "Deleting"} {
		setStatus(status)
		if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data"); code(err) != "APP_NOT_EDITABLE" {
			t.Fatalf("%s app accepted: %v", status, err)
		}
	}
	setStatus("Failed")
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data"); code(err) != "APP_CLEANUP_PENDING" {
		t.Fatalf("failed app without cleanup evidence accepted: %v", err)
	}
	revision, _ := server.NewID()
	rawSpec, _ := json.Marshal(app.Spec)
	if _, err := store.Pool.Exec(ctx, `INSERT INTO app_revisions(id,account_id,app_id,revision,platform_image_digest,spec)
		VALUES($1,$2,$3,1,$4,$5::jsonb)`, revision, owner, app.ID,
		"registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rawSpec); err != nil {
		t.Fatal(err)
	}
	run, _ := server.NewID()
	cleanup, _ := server.NewID()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,idempotency_key,desired_state,state,created_at)
		VALUES($1,$2,$3,$4,'storage-run','Running','Failed',now()-interval '2 minutes')`, run, owner, app.ID, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO deployment_tasks(id,account_id,app_id,revision_id,idempotency_key,desired_state,state,task_kind,created_at)
		VALUES($1,$2,$3,$4,'storage-cleanup','Stopped','Succeeded','Stop',now()-interval '1 minute')`, cleanup, owner, app.ID, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data"); err != nil {
		t.Fatalf("cleaned failed app could not be repaired: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET state='Queued' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, false, "", ""); code(err) != "APP_CLEANUP_PENDING" {
		t.Fatalf("active task accepted: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET state='Failed' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	for _, reservation := range []struct{ insert, remove string }{
		{`INSERT INTO resource_reservations(app_id,account_id,revision_id,cpu_request_milli,cpu_limit_milli,memory_mib,pods)
		 VALUES($1,$2,$3,250,500,512,1)`, `DELETE FROM resource_reservations WHERE app_id=$1`},
		{`INSERT INTO rollout_reservations(app_id,account_id,update_task_id,cpu_request_milli,cpu_limit_milli,memory_mib,pods)
		 VALUES($1,$2,$3,250,500,512,1)`, `DELETE FROM rollout_reservations WHERE app_id=$1`},
	} {
		foreignKey := revision
		if reservation.remove == `DELETE FROM rollout_reservations WHERE app_id=$1` {
			foreignKey = run
		}
		if _, err := store.Pool.Exec(ctx, reservation.insert, app.ID, owner, foreignKey); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, false, "", ""); code(err) != "APP_CLEANUP_PENDING" {
			t.Fatalf("unreleased reservation accepted: %v", err)
		}
		if _, err := store.Pool.Exec(ctx, reservation.remove, app.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET created_at=now() WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, false, "", ""); code(err) != "APP_CLEANUP_PENDING" {
		t.Fatalf("stale cleanup evidence accepted: %v", err)
	}
	setStatus("Stopped")
	if _, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, false, "", ""); err != nil {
		t.Fatalf("stopped app detach: %v", err)
	}
	var savedSpec []byte
	if err := store.Pool.QueryRow(ctx, `SELECT spec FROM app_revisions WHERE id=$1`, revision).Scan(&savedSpec); err != nil {
		t.Fatal(err)
	}
	var previous domain.AppSpec
	if json.Unmarshal(savedSpec, &previous) != nil || previous.Persistent {
		t.Fatal("storage edit rewrote historical revision")
	}
}

func TestConcurrentAppStorageBindingIsExclusive(t *testing.T) {
	store, owner, _ := appStorageTestStore(t)
	ctx := t.Context()
	first := appStorageTestApp(t, store, owner, "first")
	second := appStorageTestApp(t, store, owner, "second")
	volume := appStorageTestVolume(t, store, owner, "data", true)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, id := range []string{first.ID, second.ID} {
		wg.Go(func() {
			<-start
			_, err := store.UpdateAppStorage(ctx, owner, "owner", id, true, volume.ID, "/data")
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if code(err) == "VOLUME_ALREADY_BOUND" {
			rejected++
		} else {
			t.Fatalf("unexpected concurrent edit error: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("exclusive binding: accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestAppStorageWaitingForWorkerDoesNotBlockItsAudit(t *testing.T) {
	store, owner, _ := appStorageTestStore(t)
	app := appStorageTestApp(t, store, owner, "agent")
	volume := appStorageTestVolume(t, store, owner, "data", true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Rollback(context.Background())
	// A worker finishing the previous workload already owns the app row. The
	// storage edit must wait, but must not prevent its foreign-key audit insert.
	if _, err := worker.Exec(ctx, `UPDATE apps SET status='Stopped' WHERE id=$1`, app.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.UpdateAppStorage(ctx, owner, "owner", app.ID, true, volume.ID, "/data")
		result <- err
	}()
	// Wait until the edit owns the account row, establishing the lock ordering
	// deterministically instead of relying on a timing sleep.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		probe, err := store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = probe.Exec(ctx, `SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, owner)
		_ = probe.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			t.Fatalf("storage edit did not wait for worker: %v", err)
		case <-ctx.Done():
			t.Fatal("storage edit never acquired account lock")
		case <-ticker.C:
		}
	}
	if _, err := worker.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
		t.Fatal(err)
	}
	opID, _ := server.NewID()
	if _, err := worker.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state)
		VALUES($1,$2,$3,'worker','STORAGE_TEST_WORKER_FINISHED','Succeeded')`, opID, owner, app.ID); err != nil {
		t.Fatalf("storage edit blocked worker audit foreign keys: %v", err)
	}
	if err := worker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("storage edit after worker completion: %v", err)
	}
}
