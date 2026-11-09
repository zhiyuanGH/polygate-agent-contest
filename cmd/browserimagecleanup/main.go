// browserimagecleanup removes one finished, disposable student image pilot.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/postgres"
	"polygate/internal/server"
)

type config struct {
	AccountID     string   `json:"accountId"`
	ArtifactID    string   `json:"artifactId"`
	ArtifactIDs   []string `json:"artifactIds"`
	ServiceIP     string   `json:"serviceIP"`
	CACert        string   `json:"caCert"`
	AdminPassword string   `json:"adminPassword"`
	DatabaseURL   string   `json:"databaseURL"`
}

func kubectl(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s failed: %s", args[0], string(output[max(0, len(output)-600):]))
	}
	return output, nil
}

func reconcilePullCredential(ctx context.Context) error {
	id, err := server.NewID()
	if err != nil {
		return err
	}
	name := "browser-clean-" + strings.ReplaceAll(id[:13], "-", "")
	if _, err := kubectl(ctx, "-n", "polygate-system", "create", "job", name,
		"--from=cronjob/polygate-pull-secret-controller"); err != nil {
		return err
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_, _ = kubectl(clean, "-n", "polygate-system", "delete", "job", name, "--ignore-not-found")
	}()
	_, err = kubectl(ctx, "-n", "polygate-system", "wait", "--for=condition=complete",
		"job/"+name, "--timeout=180s")
	return err
}

func run() error {
	var cfg config
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 128*1024)).Decode(&cfg); err != nil {
		return err
	}
	if len(cfg.ArtifactIDs) == 0 && cfg.ArtifactID != "" {
		cfg.ArtifactIDs = []string{cfg.ArtifactID}
	}
	if cfg.AccountID == "" || len(cfg.ArtifactIDs) < 1 || len(cfg.ArtifactIDs) > 2 ||
		cfg.ServiceIP == "" || cfg.CACert == "" ||
		cfg.AdminPassword == "" || cfg.DatabaseURL == "" {
		return errors.New("cleanup configuration incomplete")
	}
	for _, id := range cfg.ArtifactIDs {
		if _, err := kubeobjects.NamespaceName(id); err != nil {
			return errors.New("invalid browser pilot artifact ID")
		}
	}
	if len(cfg.ArtifactIDs) == 2 && cfg.ArtifactIDs[0] == cfg.ArtifactIDs[1] {
		return errors.New("duplicate browser pilot artifact ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("database unavailable")
	}
	defer store.Close()
	var imageCount, pilotAppCount, reservationCount, rolloutCount, activeTasks, revisionCount int
	err = store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM image_artifacts WHERE account_id=$1 AND id=ANY($2::uuid[])),
		(SELECT count(*) FROM apps WHERE account_id=$1 AND (name LIKE 'pilot-image-%' OR name LIKE 'rollout-%')),
		(SELECT count(*) FROM resource_reservations WHERE account_id=$1),
		(SELECT count(*) FROM rollout_reservations WHERE account_id=$1),
		(SELECT count(*) FROM deployment_tasks WHERE account_id=$1 AND state IN ('Queued','Leased')),
		(SELECT count(*) FROM app_revisions WHERE account_id=$1 AND image_artifact_id=ANY($2::uuid[]))`,
		cfg.AccountID, cfg.ArtifactIDs).Scan(&imageCount, &pilotAppCount, &reservationCount,
		&rolloutCount, &activeTasks, &revisionCount)
	if err != nil {
		return fmt.Errorf("browser pilot resource check failed: %w", err)
	}
	if imageCount != len(cfg.ArtifactIDs) || pilotAppCount != 0 || reservationCount != 0 || rolloutCount != 0 ||
		activeTasks != 0 || revisionCount != 0 {
		return fmt.Errorf("browser pilot still owns resources: selected_images=%d apps=%d reservations=%d rollout=%d active=%d revisions=%d",
			imageCount, pilotAppCount, reservationCount, rolloutCount, activeTasks, revisionCount)
	}
	allowed := map[string]bool{
		"registry-1.docker.io/lipanski/docker-static-website:latest": true,
		"registry-1.docker.io/lipanski/docker-static-website:2.5.0":  true,
	}
	seen := map[string]bool{}
	digests := make([]string, 0, len(cfg.ArtifactIDs))
	for _, artifactID := range cfg.ArtifactIDs {
		var source, status, digest string
		err = store.Pool.QueryRow(ctx, `SELECT submitted_reference,scan_status,platform_digest FROM image_artifacts
			WHERE id=$1 AND account_id=$2`, artifactID, cfg.AccountID).Scan(&source, &status, &digest)
		if err != nil || !allowed[source] || status != "Approved" || seen[source] {
			return errors.New("artifact is not an approved browser pilot image")
		}
		seen[source] = true
		digests = append(digests, digest)
	}
	namespace, err := kubeobjects.NamespaceName(cfg.AccountID)
	if err != nil {
		return err
	}
	if _, err := kubectl(ctx, "get", "namespace", namespace); err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	client, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
		Username: "admin", Password: cfg.AdminPassword, CACertPEM: []byte(cfg.CACert),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == net.JoinHostPort(harbor.RegistryHost, "443") {
				address = net.JoinHostPort(cfg.ServiceIP, "443")
			}
			return dialer.DialContext(ctx, network, address)
		}})
	if err != nil {
		return errors.New("Harbor client unavailable")
	}
	if exists, err := client.AccountProjectExists(ctx, cfg.AccountID); err != nil || !exists {
		return errors.New("expected browser pilot Harbor project is missing")
	}
	// Keep application audit history; remove only image-specific disposable records.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM operations WHERE account_id=$1 AND kind IN ('IMAGE_INSPECT','IMAGE_SCAN') AND
		detail->>'artifactId'=ANY($2::text[])`, cfg.AccountID, cfg.ArtifactIDs); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM image_artifacts WHERE id=ANY($1::uuid[]) AND account_id=$2`,
		cfg.ArtifactIDs, cfg.AccountID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	var remaining, remainingApproved int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE scan_status='Approved')
		FROM image_artifacts WHERE account_id=$1`, cfg.AccountID).Scan(&remaining, &remainingApproved); err != nil {
		return err
	}
	if err = reconcilePullCredential(ctx); err != nil {
		return err
	}
	secret, err := kubectl(ctx, "-n", namespace, "get", "secret", "harbor-pull", "--ignore-not-found", "-o", "name")
	if err != nil || (len(bytes.TrimSpace(secret)) != 0) != (remainingApproved > 0) {
		return errors.New("pull Secret differs from remaining approved artifacts")
	}
	for _, digest := range digests {
		if err = client.DeleteAccountArtifact(ctx, cfg.AccountID, "agent", digest); err != nil {
			return err
		}
	}
	if remaining == 0 {
		if err = client.DeleteAccountRepository(ctx, cfg.AccountID, "agent"); err != nil {
			return err
		}
		if err = client.DeleteAccountProject(ctx, cfg.AccountID); err != nil {
			return err
		}
		if exists, err := client.AccountProjectExists(ctx, cfg.AccountID); err != nil || exists {
			return errors.New("browser pilot Harbor project remained")
		}
	}
	fmt.Printf("BROWSER_IMAGE_CLEANUP_PASS: %d test artifacts removed; %d account images remain\n",
		len(digests), remaining)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "browser image cleanup failed:", err)
		os.Exit(1)
	}
}
