package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"polygate/internal/domain"
)

func storageTestSpec(name string) domain.AppSpec {
	return domain.AppSpec{Name: name, Image: "example.com/app:v1", Port: 8000,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 500, MemoryMiB: 512, Replicas: 1}}
}

func TestAppStorageCanBeRepairedWithoutRecreatingApp(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	app, _, err := store.Create(ctx, "local-student", "owner", "storage-app", storageTestSpec("agent"))
	if err != nil {
		t.Fatal(err)
	}
	volume, _, err := store.CreateVolume(ctx, "local-student", "owner", "storage-volume", domain.VolumeSpec{Name: "data", SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	volume.State = "Ready"
	store.volumes["local-student"][volume.ID] = volume
	_, beforeUsage, _ := store.List(ctx, "local-student")
	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		NewDevHandler(store).ServeHTTP(w, httptest.NewRequest(http.MethodPatch,
			"/api/v1/apps/"+app.ID+"/storage", strings.NewReader(body)))
		return w
	}
	body := `{"persistent":true,"volumeId":"` + volume.ID + `","mountPath":"/data"}`
	for range 2 {
		if w := patch(body); w.Code != http.StatusOK {
			t.Fatalf("attach/replay: %d %s", w.Code, w.Body.String())
		}
	}
	got, err := store.Get(ctx, "local-student", app.ID)
	if err != nil || got.ID != app.ID || got.Status != "Draft" || !got.Spec.Persistent || got.Spec.VolumeID != volume.ID || got.Spec.MountPath != "/data" {
		t.Fatalf("saved binding: %+v %v", got, err)
	}
	_, afterUsage, _ := store.List(ctx, "local-student")
	if beforeUsage != afterUsage || got.Spec.Resources != app.Spec.Resources {
		t.Fatalf("storage edit changed capacity: before=%+v after=%+v", beforeUsage, afterUsage)
	}
	ops, _ := store.ListOperations(ctx, "local-student")
	count := 0
	for _, op := range ops {
		if op.Kind == "UPDATE_APP_STORAGE" {
			count++
			var detail struct{ Before, After struct{ Persistent bool } }
			if err := json.Unmarshal(op.Detail, &detail); err != nil || detail.Before.Persistent || !detail.After.Persistent {
				t.Fatalf("audit: %s %v", op.Detail, err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("unchanged replay added audit operations: %d", count)
	}
	if w := patch(`{"persistent":false}`); w.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", w.Code, w.Body.String())
	}
	got, _ = store.Get(ctx, "local-student", app.ID)
	if got.Spec.Persistent || got.Spec.VolumeID != "" || got.Spec.MountPath != "" {
		t.Fatalf("binding remains: %+v", got.Spec)
	}
	if kept, err := store.GetVolume(ctx, "local-student", volume.ID); err != nil || kept != volume {
		t.Fatalf("detach changed or removed volume: %+v %v", kept, err)
	}
}

func TestAppStorageRejectsInvalidBodiesAndUnsafeBindings(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	app, _, err := store.Create(ctx, "local-student", "owner", "storage-validation", storageTestSpec("agent"))
	if err != nil {
		t.Fatal(err)
	}
	volume, _, err := store.CreateVolume(ctx, "local-student", "owner", "storage-unready", domain.VolumeSpec{Name: "data", SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `{"persistent":null}`, `{"persistent":"false"}`, `{"persistent":false,"unexpected":1}`,
		`{"persistent":false}{}`, `{"persistent":false,"volumeId":"` + volume.ID + `"}`,
		`{"persistent":true,"volumeId":"` + volume.ID + `","mountPath":"/proc/data"}`,
		`{"persistent":true,"volumeId":"` + volume.ID + `","mountPath":"/data"}`,
	} {
		w := httptest.NewRecorder()
		NewDevHandler(store).ServeHTTP(w, httptest.NewRequest(http.MethodPatch,
			"/api/v1/apps/"+app.ID+"/storage", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if got, _ := store.Get(ctx, "local-student", app.ID); got.Spec.Persistent {
		t.Fatal("invalid edit changed the app")
	}
	foreign, _, _ := store.CreateVolume(ctx, "other", "other", "foreign-volume", domain.VolumeSpec{Name: "data", SizeGiB: 1})
	foreign.State = "Ready"
	store.volumes["other"][foreign.ID] = foreign
	if _, err := store.UpdateAppStorage(ctx, "local-student", "owner", app.ID, true, foreign.ID, "/data"); domain.AsDomainError(err) == nil {
		t.Fatal("cross-account volume attached")
	}
	if _, err := store.UpdateAppStorage(ctx, "other", "other", app.ID, false, "", ""); err != ErrNotFound {
		t.Fatalf("cross-account app edit: %v", err)
	}
}

func TestAppStorageRequiresInactiveSingleReplicaAndExclusiveVolume(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	app, _, _ := store.Create(ctx, "local-student", "owner", "storage-status", storageTestSpec("agent"))
	volume, _, _ := store.CreateVolume(ctx, "local-student", "owner", "storage-ready", domain.VolumeSpec{Name: "data", SizeGiB: 1})
	volume.State = "Ready"
	store.volumes["local-student"][volume.ID] = volume
	for _, status := range []string{"Queued", "Deploying", "Running", "Stopping", "Deleting", "Failed"} {
		app.Status = status
		store.apps["local-student"][app.ID] = app
		if _, err := store.UpdateAppStorage(ctx, "local-student", "owner", app.ID, true, volume.ID, "/data"); domain.AsDomainError(err) == nil {
			t.Errorf("storage changed while %s", status)
		}
	}
	app.Status = "Stopped"
	app.Spec.Resources.Replicas = 2
	store.apps["local-student"][app.ID] = app
	if _, err := store.UpdateAppStorage(ctx, "local-student", "owner", app.ID, true, volume.ID, "/data"); domain.AsDomainError(err) == nil || domain.AsDomainError(err).Code != "INVALID_REPLICAS" {
		t.Fatalf("multi-replica binding: %v", err)
	}
	app.Spec.Resources.Replicas = 1
	store.apps["local-student"][app.ID] = app
	if _, err := store.UpdateAppStorage(ctx, "local-student", "owner", app.ID, true, volume.ID, "/data"); err != nil {
		t.Fatalf("stopped app attach: %v", err)
	}
	other, _, err := store.Create(ctx, "local-student", "owner", "storage-second", storageTestSpec("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAppStorage(ctx, "local-student", "owner", other.ID, true, volume.ID, "/data"); domain.AsDomainError(err) == nil || domain.AsDomainError(err).Code != "VOLUME_ALREADY_BOUND" {
		t.Fatalf("exclusive volume reused: %v", err)
	}
}
