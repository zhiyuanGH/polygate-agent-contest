package postgres

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/server"
)

// Run only against a disposable database: the accepted deployments remain
// queued so a real worker must never be connected to this test database.
func TestFiftyAccountGlobalBudget(t *testing.T) {
	url := os.Getenv("POLYGATE_LOAD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_LOAD_TEST_DATABASE_URL to an isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const accounts = 50
	ids := make([]string, accounts)
	apps := make([]string, accounts)
	spec := domain.AppSpec{Name: "agent", Image: "student.invalid/ignored:latest", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 1}}
	for i := range accounts {
		id, err := server.NewID()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
            VALUES($1,$2,$2,'Load Test')`, id, "load-test-"+id)
		if err != nil {
			t.Fatal(err)
		}
		app, _, err := store.Create(ctx, id, "load-test", fmt.Sprintf("create-%d-%s", i, id), spec)
		if err != nil {
			t.Fatal(err)
		}
		apps[i] = app.ID
	}
	const digest = "docker.io/hashicorp/http-echo@sha256:2c213d6c05a0f68adfe9c7fe1a78a314e5c4fee783e2ee8592d49f10d0c4513f"
	type result struct {
		index int
		err   error
	}
	results := make(chan result, accounts)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _, err := store.QueueTrustedDeploy(ctx, ids[index], "load-test", apps[index],
				fmt.Sprintf("deploy-%d-%s", index, ids[index]), digest, 5678)
			results <- result{index: index, err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for result := range results {
		if result.err == nil {
			accepted++
			continue
		}
		if failure := domain.AsDomainError(result.err); failure != nil && failure.Code == "GLOBAL_CAPACITY_EXCEEDED" {
			rejected++
			continue
		}
		t.Fatalf("account %d unexpected admission result: %v", result.index, result.err)
	}
	if accepted != 25 || rejected != 25 {
		t.Fatalf("admission accepted=%d rejected=%d, want 25/25", accepted, rejected)
	}
	var memory, pods, reservations, queued int
	err = store.Pool.QueryRow(ctx, `SELECT reserved_memory_mib,reserved_pods FROM global_budget WHERE id=true`).Scan(&memory, &pods)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations`).Scan(&reservations)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM deployment_tasks WHERE state='Queued'`).Scan(&queued)
	if err != nil {
		t.Fatal(err)
	}
	if memory != 102400 || pods != 25 || reservations != 25 || queued != 25 {
		t.Fatalf("budget mismatch: memory=%d pods=%d reservations=%d queued=%d", memory, pods, reservations, queued)
	}
	t.Logf("50 concurrent accounts: 25 accepted at 4Gi, 25 rejected; global reservation exactly 100Gi / 25 Pods")
}
