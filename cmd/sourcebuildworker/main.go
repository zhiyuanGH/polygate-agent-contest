package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"polygate/internal/buildagent"
	"polygate/internal/harbor"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/server"
	"polygate/internal/sourcebuild"
)

func readRequired(pathEnv string) ([]byte, error) {
	path := os.Getenv(pathEnv)
	if path == "" {
		return nil, errors.New("required file path is absent")
	}
	return os.ReadFile(path)
}

func signingKey(data []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		return nil, errors.New("build signing key must be PKCS#8 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("build signing key is invalid")
	}
	return key, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if postgres.DatabaseURLFromEnvironment() == "" || os.Getenv("POLYGATE_SOURCE_ROOT") == "" ||
		os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD") == "" {
		log.Fatal("Source build worker database, source volume or Harbor credential is absent")
	}
	ca, err := readRequired("POLYGATE_HARBOR_CA_PATH")
	if err != nil {
		log.Fatal("Source build worker Harbor CA is unavailable")
	}
	clientCert, err := readRequired("POLYGATE_BUILD_CLIENT_CERT_PATH")
	if err != nil {
		log.Fatal("Source build worker client certificate is unavailable")
	}
	clientKey, err := readRequired("POLYGATE_BUILD_CLIENT_KEY_PATH")
	if err != nil {
		log.Fatal("Source build worker client key is unavailable")
	}
	agentCA, err := readRequired("POLYGATE_BUILD_AGENT_CA_PATH")
	if err != nil {
		log.Fatal("Source build worker Agent CA is unavailable")
	}
	signBytes, err := readRequired("POLYGATE_BUILD_SIGNING_KEY_PATH")
	if err != nil {
		log.Fatal("Source build worker signing key is unavailable")
	}
	key, err := signingKey(signBytes)
	if err != nil {
		log.Fatal("Source build worker signing key is invalid")
	}
	workerTLS, err := buildagent.WorkerTLSConfig(clientCert, clientKey, agentCA,
		os.Getenv("POLYGATE_BUILD_AGENT_SERVER_NAME"))
	if err != nil {
		log.Fatal("Source build worker Agent TLS configuration is invalid")
	}
	agent, err := buildagent.NewClient(os.Getenv("POLYGATE_BUILD_AGENT_URL"), workerTLS, key)
	if err != nil {
		log.Fatal("Source build worker Agent endpoint is invalid")
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(ca) {
		log.Fatal("Source build worker Harbor CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}}
	resolver, err := registry.NewResolver(registry.Policy{Registries: []registry.Registry{
		{Host: harbor.RegistryHost}}}, transport)
	if err != nil {
		log.Fatal("Source build worker Harbor resolver is invalid")
	}
	harborClient, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
		Username: "admin", Password: os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD"), CACertPEM: ca})
	if err != nil {
		log.Fatal("Source build worker Harbor connection is invalid")
	}
	openCtx, finish := context.WithTimeout(ctx, 15*time.Second)
	store, err := postgres.Open(openCtx, postgres.DatabaseURLFromEnvironment())
	finish()
	if err != nil {
		log.Fatal("Source build worker database connection failed")
	}
	defer store.Close()
	id, err := server.NewID()
	if err != nil {
		log.Fatal("Source build worker identity failed")
	}
	worker := sourcebuild.Worker{Store: store, Agent: agent,
		Verifier:   harbor.BuildVerifier{Harbor: harborClient, Resolver: resolver},
		SourceRoot: os.Getenv("POLYGATE_SOURCE_ROOT"), ID: id}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	log.Print("PolyGate source build worker started")
	for {
		processed, workErr := worker.RunOnce(ctx)
		if workErr != nil {
			log.Printf("Source build reconciliation: %v", workErr)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			log.Print("PolyGate source build worker stopped")
			return
		case <-ticker.C:
		}
	}
}
