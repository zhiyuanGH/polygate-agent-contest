package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
	volumejob "polygate/internal/volume"
)

func TestAppVolumeBindingIsExclusiveAndSurvivesAppDeletion(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to an isolated migrated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, _ := server.NewID()
	b, _ := server.NewID()
	for _, id := range []string{a, b} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,volume_enabled)
			VALUES($1,$2,$2,'Volume Binding Test',true)`, id, "binding-test-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanup := context.Background()
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM student_volumes WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{a, b})
	}()
	volume, _, err := store.CreateVolume(ctx, a, "owner", "binding-volume-key", domain.VolumeSpec{Name: "data", SizeGiB: 5})
	if err != nil {
		t.Fatal(err)
	}
	spec := domain.AppSpec{Name: "agent", Image: "example.com/agent:v1", Port: 8080,
		Resources:  domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1},
		Persistent: true, VolumeID: volume.ID, MountPath: "/data"}
	if _, _, err := store.Create(ctx, b, "other", "other-volume-app", spec); domain.AsDomainError(err) == nil {
		t.Fatalf("cross-account volume binding: %v", err)
	}
	if _, _, err := store.Create(ctx, a, "owner", "before-ready-app", spec); domain.AsDomainError(err) == nil {
		t.Fatalf("unready volume binding: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE student_volumes SET state='Ready' WHERE id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	app, replay, err := store.Create(ctx, a, "owner", "ready-volume-app", spec)
	if err != nil || replay {
		t.Fatalf("bind ready volume: %+v %t %v", app, replay, err)
	}
	if got, err := store.Get(ctx, a, app.ID); err != nil || got.Spec.VolumeID != volume.ID || got.Spec.MountPath != "/data" {
		t.Fatalf("binding not durable: %+v %v", got, err)
	}
	if _, err := store.ExpandVolume(ctx, b, "other", volume.ID, 8); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account expansion: %v", err)
	}
	if _, err := store.ExpandVolume(ctx, a, "owner", volume.ID, 4); code(err) != "INVALID_VOLUME_SIZE" {
		t.Fatalf("shrink accepted: %v", err)
	}
	if enlarged, err := store.ExpandVolume(ctx, a, "owner", volume.ID, 8); err != nil || enlarged.State != "Expanding" {
		t.Fatalf("expand: %+v %v", enlarged, err)
	}
	if _, err := store.ExpandVolume(ctx, a, "owner", volume.ID, 11); code(err) != "INVALID_VOLUME_SIZE" {
		t.Fatalf("oversize accepted: %v", err)
	}
	claimed, found, err := store.ClaimNextVolume(ctx, "expand-worker", 60*time.Second)
	if err != nil || !found || claimed.ID != volume.ID || claimed.SizeGiB != 8 {
		t.Fatalf("expansion claim: %+v %t %v", claimed, found, err)
	}
	if err := store.CompleteVolume(ctx, volume.ID, "expand-worker"); err != nil {
		t.Fatalf("complete expansion: %v", err)
	}
	if enlarged, err := store.GetVolume(ctx, a, volume.ID); err != nil || enlarged.State != "Ready" || enlarged.Spec.SizeGiB != 8 {
		t.Fatalf("expanded volume record: %+v %v", enlarged, err)
	}
	other := spec
	other.Name = "agent-two"
	if _, _, err := store.Create(ctx, a, "owner", "duplicate-volume-app", other); code(err) != "VOLUME_ALREADY_BOUND" {
		t.Fatalf("second app bound same volume: %v", err)
	}
	if err := store.Delete(ctx, a, "owner", app.ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if got, err := store.GetVolume(ctx, a, volume.ID); err != nil || got.State != "Ready" {
		t.Fatalf("volume lost after app deletion: %+v %v", got, err)
	}
	if _, err := store.Get(ctx, a, app.ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("deleted app remains: %v", err)
	}
	rebound, _, err := store.Create(ctx, a, "owner", "rebind-after-delete", other)
	if err != nil {
		t.Fatalf("volume cannot be rebound after app deletion: %v", err)
	}
	if _, err := store.DeleteVolume(ctx, a, "owner", volume.ID, volume.Spec.Name); code(err) != "VOLUME_ALREADY_BOUND" {
		t.Fatalf("bound volume deleted: %v", err)
	}
	if err := store.Delete(ctx, a, "owner", rebound.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteVolume(ctx, b, "other", volume.ID, volume.Spec.Name); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account volume deletion: %v", err)
	}
	if _, err := store.DeleteVolume(ctx, a, "owner", volume.ID, "wrong"); code(err) != "VOLUME_CONFIRMATION_MISMATCH" {
		t.Fatalf("wrong deletion confirmation: %v", err)
	}
	deleting, err := store.DeleteVolume(ctx, a, "owner", volume.ID, volume.Spec.Name)
	if err != nil || deleting.State != "Deleting" {
		t.Fatalf("deletion request: %+v %v", deleting, err)
	}
	if _, err := store.DeleteVolume(ctx, a, "owner", volume.ID, volume.Spec.Name); err != nil {
		t.Fatalf("deletion replay: %v", err)
	}
	claimed, found, err = store.ClaimNextVolume(ctx, "delete-worker", 60*time.Second)
	if err != nil || !found || claimed.ID != volume.ID || claimed.State != "Deleting" {
		t.Fatalf("claim deletion: %+v %t %v", claimed, found, err)
	}
	target := volumejob.Task{ID: volume.ID, AccountID: a, State: "Deleting",
		PVCUID: "11111111-1111-4111-8111-111111111111",
		PVName: "pvc-22222222-2222-4222-8222-222222222222",
		Handle: "pvc-22222222-2222-4222-8222-222222222222"}
	if err := store.RecordDeletionTarget(ctx, target, "foreign-worker"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("foreign worker recorded deletion target: %v", err)
	}
	if err := store.RecordDeletionTarget(ctx, target, "delete-worker"); err != nil {
		t.Fatalf("record deletion target: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE student_volumes SET lease_until=now()-interval '1 second' WHERE id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err = store.ClaimNextVolume(ctx, "delete-worker-restarted", 60*time.Second)
	if err != nil || !found || claimed.PVCUID != target.PVCUID || claimed.PVName != target.PVName || claimed.Handle != target.Handle {
		t.Fatalf("deletion target lost across lease/restart: %+v %t %v", claimed, found, err)
	}
	if err := store.CompleteDeletion(ctx, target, "delete-worker"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("stale worker completed deletion: %v", err)
	}
	if err := store.CompleteDeletion(ctx, target, "delete-worker-restarted"); err != nil {
		t.Fatalf("complete deletion: %v", err)
	}
	if _, err := store.GetVolume(ctx, a, volume.ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("deleted volume row remains: %v", err)
	}
}

func code(err error) string {
	if e := domain.AsDomainError(err); e != nil {
		return e.Code
	}
	return ""
}
