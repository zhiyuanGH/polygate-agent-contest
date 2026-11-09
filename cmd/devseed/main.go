package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"polygate/internal/postgres"
	"polygate/internal/server"
)

type testIdentity struct {
	Students []struct {
		ID          string `json:"id"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	} `json:"students"`
}

func main() {
	if os.Getenv("POLYGATE_DEV_MODE") != "1" {
		log.Fatal("Development seeding requires POLYGATE_DEV_MODE=1")
	}
	path := os.Getenv("POLYGATE_DEV_REALM_CONFIG")
	if path == "" {
		log.Fatal("POLYGATE_DEV_REALM_CONFIG is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("Could not read the local test identity file")
	}
	var identities testIdentity
	if err = json.Unmarshal(data, &identities); err != nil || len(identities.Students) != 2 {
		log.Fatal("Expected two local test identities")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("POLYGATE_DATABASE_URL"))
	if err != nil {
		log.Fatal("Development database unavailable")
	}
	defer store.Close()
	for _, student := range identities.Students {
		id, err := server.NewID()
		if err != nil {
			log.Fatal(err)
		}
		_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name,role)
            VALUES($1,$2,$3,$4,'student')
            ON CONFLICT (oidc_subject) DO UPDATE SET username=EXCLUDED.username,
                display_name=EXCLUDED.display_name`, id, student.ID, student.Username, student.DisplayName)
		if err != nil {
			log.Fatal("Could not create local test account")
		}
	}
	log.Print("Two local test accounts are mapped to Keycloak identities.")
}
