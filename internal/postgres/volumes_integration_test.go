package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

func TestVolumeReservationsAreDurableAccountBoundAndSerialized(t *testing.T) {
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
	defer func() { store.Close() }()
	a, _ := server.NewID()
	b, _ := server.NewID()
	for _, id := range []string{a, b} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,volume_enabled)
			VALUES($1,$2,$2,'Volume Test',true)`, id, "volume-test-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanup := context.Background()
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM student_volumes WHERE account_id=ANY($1::uuid[])`, []string{a, b})
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{a, b})
	}()
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=false WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateVolume(ctx, b, "b", "disabled-volume-key", domain.VolumeSpec{Name: "blocked", SizeGiB: 1}); !errors.Is(err, server.ErrVolumeDisabled) {
		t.Fatalf("disabled pilot created volume: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=true WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	first, replay, err := store.CreateVolume(ctx, a, "a", "volume-first-key", domain.VolumeSpec{Name: "data", SizeGiB: 5})
	if err != nil || replay || first.State != "Reserved" {
		t.Fatalf("create first: %+v %t %v", first, replay, err)
	}
	if _, err := store.GetVolume(ctx, b, first.ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account get: %v", err)
	}
	if items, err := store.ListVolumes(ctx, b); err != nil || len(items) != 0 {
		t.Fatalf("cross-account list: %+v %v", items, err)
	}
	if same, replay, err := store.CreateVolume(ctx, a, "a", "volume-first-key", first.Spec); err != nil || !replay || same.ID != first.ID {
		t.Fatalf("idempotent replay: %+v %t %v", same, replay, err)
	}
	if _, _, err := store.CreateVolume(ctx, a, "a", "volume-first-key", domain.VolumeSpec{Name: "data", SizeGiB: 6}); domain.AsDomainError(err) == nil {
		t.Fatalf("changed replay: %v", err)
	}
	if _, _, err := store.CreateVolume(ctx, a, "a", "volume-duplicate-key", first.Spec); domain.AsDomainError(err) == nil {
		t.Fatalf("duplicate name: %v", err)
	}
	claimed, found, err := store.ClaimNextVolume(ctx, "volume-worker-a", 60*time.Second)
	if err != nil || !found || claimed.ID != first.ID {
		t.Fatalf("claim: %+v %t %v", claimed, found, err)
	}
	if err := store.RetryVolume(ctx, claimed.ID, "volume-worker-a", 10*time.Second); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE student_volumes SET lease_until=now()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err = store.ClaimNextVolume(ctx, "volume-worker-b", 60*time.Second)
	if err != nil || !found || claimed.ID != first.ID {
		t.Fatalf("reclaim after retry: %+v %t %v", claimed, found, err)
	}
	if err := store.CompleteVolume(ctx, claimed.ID, "volume-worker-a"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("stale worker completed claim: %v", err)
	}
	if err := store.CompleteVolume(ctx, claimed.ID, "volume-worker-b"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if ready, err := store.GetVolume(ctx, a, first.ID); err != nil || ready.State != "Ready" {
		t.Fatalf("ready state: %+v %v", ready, err)
	}
	var wg sync.WaitGroup
	accepted := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a'+i)) + "-volume"
			key := "parallel-volume-" + name
			_, _, err := store.CreateVolume(ctx, a, "a", key, domain.VolumeSpec{Name: name, SizeGiB: 5})
			accepted <- err == nil
		}(i)
	}
	wg.Wait()
	close(accepted)
	count := 0
	for success := range accepted {
		if success {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent reservations accepted %d, want 1", count)
	}
	store.Close()
	store, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetVolume(ctx, a, first.ID); err != nil || got.ID != first.ID {
		t.Fatalf("reservation lost after reconnect: %+v %v", got, err)
	}
}
