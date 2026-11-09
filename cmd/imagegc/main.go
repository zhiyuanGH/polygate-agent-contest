// imagegc reclaims old, unreferenced account images after a durable DB claim.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"polygate/internal/harbor"
	"polygate/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	ca, err := os.ReadFile(os.Getenv("POLYGATE_HARBOR_CA_PATH"))
	if err != nil {
		log.Fatal("Harbor CA unavailable")
	}
	client, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
		Username: "admin", Password: os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD"), CACertPEM: ca})
	if err != nil {
		log.Fatal("Harbor client unavailable: ", err)
	}
	store, err := postgres.Open(ctx, postgres.DatabaseURLFromEnvironment())
	if err != nil {
		log.Fatal("database unavailable")
	}
	defer store.Close()
	removed := 0
	requestedOnly := os.Getenv("POLYGATE_IMAGE_GC_MODE") == "requested"
	for removed < 100 {
		var item postgres.ImageGCCandidate
		var found bool
		if requestedOnly {
			item, found, err = store.ClaimRequestedImage(ctx, "")
		} else {
			item, found, err = store.ClaimExpiredImage(ctx, "")
		}
		if err != nil {
			log.Fatal("image retention claim failed: ", err)
		}
		if !found {
			break
		}
		if err = client.DeleteAccountArtifact(ctx, item.AccountID, "agent", item.Digest); err != nil {
			log.Fatal("Harbor image retention deletion failed: ", err)
		}
		if err = store.FinishExpiredImage(ctx, item); err != nil {
			log.Fatal("image retention database finalization failed: ", err)
		}
		removed++
	}
	log.Printf("image gc: requested_only=%t deleted=%d max_per_run=100", requestedOnly, removed)
}
