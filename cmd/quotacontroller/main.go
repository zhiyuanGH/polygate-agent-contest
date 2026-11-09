package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"polygate/internal/adminquota"
	"polygate/internal/postgres"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	url := postgres.DatabaseURLFromEnvironment()
	if url == "" {
		log.Fatal("Database configuration is required")
	}
	cluster, err := adminquota.InCluster()
	if err != nil {
		log.Fatal("Kubernetes credentials are unavailable: ", err)
	}
	openCtx, done := context.WithTimeout(ctx, 15*time.Second)
	store, err := postgres.Open(openCtx, url)
	done()
	if err != nil {
		log.Fatal("Database connection failed: ", err)
	}
	defer store.Close()
	log.Print("PolyGate account quota controller started")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	nextDriftCheck := time.Time{}
	for {
		workCtx, finish := context.WithTimeout(ctx, 45*time.Second)
		processed, err := store.ProcessNextQuotaChange(workCtx, cluster.Apply)
		finish()
		if err != nil {
			log.Printf("Quota change failed and remains queued: %v", err)
		}
		if processed && err == nil {
			continue
		}
		if time.Now().After(nextDriftCheck) {
			checkCtx, complete := context.WithTimeout(ctx, 2*time.Minute)
			targets, listErr := store.ListAccountQuotaTargets(checkCtx)
			if listErr != nil {
				log.Printf("Quota drift scan failed: %v", listErr)
			} else {
				for _, target := range targets {
					if applyErr := cluster.Apply(checkCtx, target.AccountID, target.Quota); applyErr != nil {
						log.Printf("Quota drift repair failed for account %s: %v", target.AccountID, applyErr)
						if checkCtx.Err() != nil {
							break
						}
					}
				}
			}
			complete()
			nextDriftCheck = time.Now().Add(5 * time.Minute)
		}
		select {
		case <-ctx.Done():
			log.Print("PolyGate account quota controller stopped")
			return
		case <-ticker.C:
		}
	}
}
