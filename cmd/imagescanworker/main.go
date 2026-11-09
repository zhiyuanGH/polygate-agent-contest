package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"polygate/internal/harbor"
	"polygate/internal/imagescan"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := postgres.DatabaseURLFromEnvironment()
	if databaseURL == "" {
		log.Fatal("Image scanner database configuration is required")
	}
	ca, err := os.ReadFile(os.Getenv("POLYGATE_HARBOR_CA_PATH"))
	if err != nil {
		log.Fatal("Image scanner Harbor CA is unavailable")
	}
	client, err := harbor.New(harbor.Config{
		BaseURL: "https://" + harbor.RegistryHost, Username: "admin",
		Password: os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD"), CACertPEM: ca,
	})
	if err != nil {
		log.Fatal("Image scanner Harbor connection is invalid: ", err)
	}
	decoder := json.NewDecoder(strings.NewReader(os.Getenv("POLYGATE_REGISTRY_POLICY_JSON")))
	decoder.DisallowUnknownFields()
	var policy registry.Policy
	if decoder.Decode(&policy) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		log.Fatal("Image scanner registry policy is invalid")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal("Image scanner system certificate pool is unavailable")
	}
	if !roots.AppendCertsFromPEM(ca) {
		log.Fatal("Image scanner Harbor CA is invalid")
	}
	// Preserve Go's default dial and TLS timeouts. In particular, an idle
	// Docker Hub auth connection must not remain reusable indefinitely; a
	// stalled token response would otherwise hold the only worker for 13m.
	resolverTransport := http.DefaultTransport.(*http.Transport).Clone()
	resolverTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	resolverTransport.ResponseHeaderTimeout = 30 * time.Second
	resolverTransport.IdleConnTimeout = 30 * time.Second
	resolver, err := registry.NewResolver(policy, resolverTransport)
	if err != nil {
		log.Fatal("Image scanner registry policy is invalid: ", err)
	}
	openCtx, done := context.WithTimeout(ctx, 15*time.Second)
	store, err := postgres.Open(openCtx, databaseURL)
	done()
	if err != nil {
		log.Fatal("Image scanner database connection failed")
	}
	defer store.Close()
	if len(os.Args) == 2 && os.Args[1] == "--backfill-reports" {
		backfillCtx, finish := context.WithTimeout(ctx, 10*time.Minute)
		defer finish()
		if err := backfillReports(backfillCtx, store, client); err != nil {
			log.Fatal("Image scan report backfill failed: ", err)
		}
		return
	}
	id, err := server.NewID()
	if err != nil {
		log.Fatal("Image scanner worker identity failed")
	}
	worker := imagescan.Worker{Store: store, Harbor: client, Resolver: resolver, ID: id}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	log.Print("PolyGate image scanner worker started")
	for {
		workCtx, finish := context.WithTimeout(ctx, imagescan.WorkTimeout)
		processed, err := worker.RunOnce(workCtx)
		finish()
		if err != nil {
			log.Printf("Image scan reconciliation: %v", err)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			log.Print("PolyGate image scanner worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func backfillReports(ctx context.Context, store *postgres.Store, client *harbor.Client) error {
	items, err := store.ListUnarchivedScanReports(ctx, 1000)
	if err != nil {
		return err
	}
	archived := 0
	failed := 0
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		report, err := client.ReadVulnerabilityReport(ctx, item.HarborReference,
			item.ReportID, item.ReportSHA256)
		if err == nil {
			err = store.ArchiveImageScanReport(ctx, item.AccountID, item.ArtifactID,
				item.HarborReference, item.ReportID, report)
		}
		if err != nil {
			failed++
			log.Printf("Image scan report backfill failed artifact=%s: %v", item.ArtifactID, err)
			continue
		}
		archived++
	}
	log.Printf("IMAGE_SCAN_REPORT_BACKFILL archived=%d failed=%d total=%d", archived, failed, len(items))
	if failed > 0 || len(items) == 1000 {
		return fmt.Errorf("image scan report backfill incomplete: failed=%d fetched=%d", failed, len(items))
	}
	return nil
}
