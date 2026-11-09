package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"polygate/internal/postgres"
	"polygate/internal/server"
	"polygate/internal/volume"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	url := postgres.DatabaseURLFromEnvironment()
	if url == "" {
		log.Fatal("Database configuration is required")
	}
	cluster, err := volume.NewKubernetes(os.Getenv("POLYGATE_VOLUME_KUBECONFIG"))
	if err != nil {
		log.Fatal("Kubernetes credentials are invalid: ", err)
	}
	openCtx, done := context.WithTimeout(ctx, 15*time.Second)
	store, err := postgres.Open(openCtx, url)
	done()
	if err != nil {
		log.Fatal("Database connection failed: ", err)
	}
	defer store.Close()
	id, err := server.NewID()
	if err != nil {
		log.Fatal("Could not create worker ID: ", err)
	}
	worker := volume.Worker{Store: store, Cluster: cluster, ID: id, Lease: 2 * time.Minute}
	log.Print("PolyGate volume controller started")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		workCtx, finish := context.WithTimeout(ctx, 45*time.Second)
		processed, err := worker.RunOnce(workCtx)
		finish()
		if err != nil {
			log.Printf("Volume reconciliation failed: %v", err)
		}
		if processed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
