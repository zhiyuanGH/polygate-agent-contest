// registryprobe verifies one operator-approved public image reference without
// pulling layers or deploying a workload.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"polygate/internal/registry"
)

func main() {
	ref := flag.String("ref", "", "fully qualified image reference")
	approved := flag.String("registry", "", "operator-approved registry hostname")
	tokenHosts := flag.String("token-hosts", "", "comma-separated approved token hostnames")
	blobHosts := flag.String("blob-hosts", "", "comma-separated approved blob mirror hostnames")
	flag.Parse()
	if *ref == "" || *approved == "" {
		log.Fatal("-ref and -registry are required")
	}
	entry := registry.Registry{Host: *approved}
	if *tokenHosts != "" {
		entry.AuthHosts = strings.Split(*tokenHosts, ",")
	}
	if *blobHosts != "" {
		entry.BlobHosts = strings.Split(*blobHosts, ",")
	}
	policy := registry.Policy{Registries: []registry.Registry{entry}}
	resolver, err := registry.NewResolver(policy, nil)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	image, err := resolver.Resolve(ctx, *ref, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(image); err != nil {
		log.Fatal(err)
	}
}
