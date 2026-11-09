// clusterload exercises the production queue and resident worker with only
// disposable, platform-approved sample workloads. The operator creates the
// matching tenant namespaces before invoking this command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	"polygate/internal/domain"
	"polygate/internal/postgres"
)

const image = "docker.io/hashicorp/http-echo@sha256:2c213d6c05a0f68adfe9c7fe1a78a314e5c4fee783e2ee8592d49f10d0c4513f"

type account struct {
	ID  string
	App string
}

func main() {
	mode := os.Getenv("POLYGATE_LOAD_MODE")
	runID := os.Getenv("POLYGATE_LOAD_RUN_ID")
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(runID) || (mode != "run" && mode != "cleanup") {
		log.Fatal("load mode or run ID is invalid")
	}
	var ids []string
	if err := json.Unmarshal([]byte(os.Getenv("POLYGATE_LOAD_ACCOUNTS")), &ids); err != nil || len(ids) != 50 {
		log.Fatal("exactly 50 test account IDs are required")
	}
	seen := make(map[string]bool)
	idPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	for _, id := range ids {
		if !idPattern.MatchString(id) || seen[id] {
			log.Fatal("invalid or duplicate account ID")
		}
		seen[id] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	store, err := postgres.Open(ctx, postgres.DatabaseURLFromEnvironment())
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer store.Close()
	if mode == "run" {
		err = start(ctx, store, runID, ids)
	} else {
		err = cleanup(ctx, store, runID, ids)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func start(ctx context.Context, store *postgres.Store, runID string, ids []string) error {
	accounts := make([]account, len(ids))
	spec := domain.AppSpec{Name: "load-" + runID, Image: "student.invalid/ignored:latest", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 1}}
	for i, id := range ids {
		identity := fmt.Sprintf("load-%s-%02d", runID, i)
		_, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
            VALUES($1,$2,$2,'Load Test') ON CONFLICT (id) DO NOTHING`, id, identity)
		if err != nil {
			return fmt.Errorf("account %d: %w", i, err)
		}
		app, _, err := store.Create(ctx, id, identity, "create-"+runID, spec)
		if err != nil {
			return fmt.Errorf("draft %d: %w", i, err)
		}
		accounts[i] = account{ID: id, App: app.ID}
	}
	log.Print("Created 50 isolated account drafts")
	type result struct {
		Index int
		Err   error
	}
	results := make(chan result, len(accounts))
	var wg sync.WaitGroup
	started := time.Now()
	for i := range accounts {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			item := accounts[index]
			_, _, err := store.QueueTrustedDeploy(ctx, item.ID, "load-test", item.App,
				"deploy-"+runID, image, 5678)
			results <- result{Index: index, Err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := make([]account, 0, 25)
	rejected := 0
	for item := range results {
		if item.Err == nil {
			accepted = append(accepted, accounts[item.Index])
			continue
		}
		if failure := domain.AsDomainError(item.Err); failure != nil && failure.Code == "GLOBAL_CAPACITY_EXCEEDED" {
			rejected++
			continue
		}
		return fmt.Errorf("admission %d: %w", item.Index, item.Err)
	}
	if len(accepted) != 25 || rejected != 25 {
		return fmt.Errorf("admission accepted=%d rejected=%d, expected 25/25", len(accepted), rejected)
	}
	log.Printf("ADMISSION_25 accepted=25 rejected=25 elapsed=%s", time.Since(started).Round(time.Millisecond))
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		running := 0
		for _, item := range accepted {
			app, err := store.Get(ctx, item.ID, item.App)
			if err != nil {
				return err
			}
			if app.Status == "Failed" {
				return fmt.Errorf("app %s failed before Running", item.App)
			}
			if app.Status == "Running" {
				running++
			}
		}
		log.Printf("Running %d/25", running)
		if running == 25 {
			log.Printf("RUNNING_25 elapsed=%s", time.Since(started).Round(time.Second))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return errors.New("timed out waiting for 25 Running apps")
}

func cleanup(ctx context.Context, store *postgres.Store, runID string, ids []string) error {
	name := "load-" + runID
	for _, id := range ids {
		apps, _, err := store.List(ctx, id)
		if err != nil {
			return err
		}
		for _, app := range apps {
			if app.Spec.Name != name {
				return fmt.Errorf("unexpected app %s in test account", app.Spec.Name)
			}
			if app.Status == "Queued" || app.Status == "Deploying" || app.Status == "Running" {
				var stopErr error
				for attempt := 0; attempt < 10; attempt++ {
					_, _, stopErr = store.QueueStop(ctx, id, "load-test", app.ID, "stop-"+runID)
					if stopErr == nil {
						break
					}
					if failure := domain.AsDomainError(stopErr); failure == nil || failure.Code != "TASK_BUSY" {
						break
					}
					time.Sleep(2 * time.Second)
				}
				if stopErr != nil {
					return fmt.Errorf("stop %s: %w", app.ID, stopErr)
				}
			}
		}
	}
	log.Print("Stop requests submitted for all accepted apps")
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		pending := 0
		for _, id := range ids {
			apps, _, err := store.List(ctx, id)
			if err != nil {
				return err
			}
			for _, app := range apps {
				if app.Status != "Stopped" && app.Status != "Draft" {
					pending++
				}
			}
		}
		log.Printf("Cleanup pending apps: %d", pending)
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	for _, id := range ids {
		apps, _, err := store.List(ctx, id)
		if err != nil {
			return err
		}
		for _, app := range apps {
			if app.Status != "Stopped" && app.Status != "Draft" {
				return fmt.Errorf("app %s still has status %s", app.ID, app.Status)
			}
			if err := store.Delete(ctx, id, "load-test", app.ID); err != nil {
				return err
			}
		}
		if _, err := store.Pool.Exec(ctx, `DELETE FROM operations WHERE account_id=$1`, id); err != nil {
			return err
		}
		if _, err := store.Pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1 AND username LIKE $2`, id, "load-"+runID+"-%"); err != nil {
			return err
		}
	}
	var reservations, active, remaining int
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_reservations`).Scan(&reservations); err != nil {
		return err
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM deployment_tasks WHERE state IN ('Queued','Leased')`).Scan(&active); err != nil {
		return err
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM accounts WHERE username LIKE $1`, "load-"+runID+"-%").Scan(&remaining); err != nil {
		return err
	}
	if reservations != 0 || active != 0 || remaining != 0 {
		return fmt.Errorf("cleanup incomplete: reservations=%d active=%d accounts=%d", reservations, active, remaining)
	}
	log.Print("CLEANUP_COMPLETE: no test accounts, apps, active tasks or reservations")
	return nil
}
