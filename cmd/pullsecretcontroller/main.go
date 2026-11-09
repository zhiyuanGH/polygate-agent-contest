package main

import (
	"context"
	"log"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/harbor"
	"polygate/internal/postgres"
	"polygate/internal/pullsecret"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	ca, err := os.ReadFile(os.Getenv("POLYGATE_HARBOR_CA_PATH"))
	if err != nil {
		log.Fatal("Harbor CA is unavailable")
	}
	harborClient, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
		Username: "admin", Password: os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD"),
		CACertPEM: ca})
	if err != nil {
		log.Fatal("Harbor pull controller configuration is invalid")
	}
	db, err := postgres.Open(ctx, postgres.DatabaseURLFromEnvironment())
	if err != nil {
		log.Fatal("Pull controller database is unavailable")
	}
	defer db.Close()
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("Pull controller Kubernetes credentials are unavailable")
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal("Pull controller Kubernetes client could not start")
	}
	controller := pullsecret.Controller{DB: db, Harbor: harborClient,
		Secrets: pullsecret.KubernetesStore{Client: client}}
	if err := controller.Run(ctx); err != nil {
		// Errors from external services may include request details. Keep the
		// failure signal, but never print registry credentials or Secret data.
		log.Fatal("Pull credential reconciliation failed")
	}
	log.Print("Pull credential reconciliation complete")
}
