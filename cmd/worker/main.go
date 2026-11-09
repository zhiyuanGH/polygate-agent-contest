package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"polygate/internal/deploy"
	"polygate/internal/postgres"
	"polygate/internal/server"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	databaseURL := postgres.DatabaseURLFromEnvironment()
	if databaseURL == "" {
		log.Fatal("Database configuration is required")
	}
	provisionerPath := os.Getenv("POLYGATE_PROVISIONER_KUBECONFIG")
	workloadPath := os.Getenv("POLYGATE_WORKLOAD_KUBECONFIG")
	var cluster *deploy.KubernetesCluster
	var err error
	if os.Getenv("POLYGATE_TENANTS_PRECREATED") == "1" {
		cluster, err = deploy.NewPrecreatedKubernetesCluster(workloadPath)
	} else {
		cluster, err = deploy.NewKubernetesCluster(provisionerPath, workloadPath)
	}
	if err != nil {
		log.Fatal("Kubernetes credentials are invalid: ", err)
	}
	openCtx, done := context.WithTimeout(ctx, 15*time.Second)
	store, err := postgres.Open(openCtx, databaseURL)
	done()
	if err != nil {
		log.Fatal("Database connection failed; run migrations and check POLYGATE_DATABASE_URL")
	}
	defer store.Close()
	id, err := server.NewID()
	if err != nil {
		log.Fatal("Could not create worker ID: ", err)
	}
	worker := deploy.Worker{Store: store, Cluster: cluster, ID: id, Lease: 2 * time.Minute}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	log.Print("PolyGate trusted workload worker started")
	for {
		workCtx, finish := context.WithTimeout(ctx, 45*time.Second)
		processed, err := worker.RunOnce(workCtx)
		finish()
		if err != nil {
			log.Printf("Deployment reconciliation failed: %v", err)
		}
		if processed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			log.Print("PolyGate worker stopped")
			return
		case <-ticker.C:
		}
	}
}
