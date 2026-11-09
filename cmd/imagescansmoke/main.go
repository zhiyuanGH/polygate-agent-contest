// imagescansmoke exercises one durable scan task against the live pilot and
// removes its temporary account, evidence and Harbor project afterward.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"polygate/internal/harbor"
	"polygate/internal/imagescan"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/server"
)

type config struct {
	ServiceIP       string `json:"serviceIP"`
	CACert          string `json:"caCert"`
	AdminPassword   string `json:"adminPassword"`
	DatabaseURL     string `json:"databaseURL"`
	Resident        bool   `json:"resident"`
	ExpectFailure   bool   `json:"expectFailure"`
	HarborSource    bool   `json:"harborSource"`
	SourceReference string `json:"sourceReference"`
	ExpectStatus    string `json:"expectStatus"`
	GCSmoke         bool   `json:"gcSmoke"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "image scan smoke failed:", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	var cfg config
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&cfg); err != nil {
		return errors.New("image scan smoke configuration is invalid")
	}
	if net.ParseIP(cfg.ServiceIP) == nil || cfg.AdminPassword == "" || cfg.DatabaseURL == "" {
		return errors.New("image scan smoke configuration is incomplete")
	}
	if cfg.SourceReference == "" {
		cfg.SourceReference = "registry-1.docker.io/library/busybox:latest"
	}
	if cfg.ExpectStatus == "" {
		cfg.ExpectStatus = "Approved"
	}
	if cfg.ExpectStatus != "Approved" && cfg.ExpectStatus != "Rejected" && cfg.ExpectStatus != "Any" {
		return errors.New("invalid expected scan status")
	}
	ctx, cancel := context.WithTimeout(context.Background(), imagescan.WorkTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == net.JoinHostPort(harbor.RegistryHost, "443") {
			address = net.JoinHostPort(cfg.ServiceIP, "443")
		}
		return dialer.DialContext(ctx, network, address)
	}
	client, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
		Username: "admin", Password: cfg.AdminPassword, CACertPEM: []byte(cfg.CACert),
		DialContext: dial})
	if err != nil {
		return errors.New("verified Harbor client could not start")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || !roots.AppendCertsFromPEM([]byte(cfg.CACert)) {
		return errors.New("verified Harbor CA is unavailable")
	}
	transport := &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots}}
	resolver, err := registry.NewResolver(registry.Policy{Registries: []registry.Registry{{
		Host: "registry-1.docker.io", AuthHosts: []string{"auth.docker.io"},
		BlobHosts: []string{"production.cloudfront.docker.com"}},
		{Host: harbor.RegistryHost}}}, transport)
	if err != nil {
		return err
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("production database is unavailable")
	}
	defer store.Close()
	var pending int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM image_scan_tasks
        WHERE state IN ('Queued','Leased')`).Scan(&pending); err != nil || pending != 0 {
		return fmt.Errorf("scan smoke requires an empty production queue (count %d)", pending)
	}
	accountID, err := server.NewID()
	if err != nil {
		return err
	}
	created := false
	defer func() {
		if !created {
			return
		}
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		cleanupFailed := false
		if _, err := store.Pool.Exec(cleanup, `DELETE FROM operations WHERE account_id=$1`, accountID); err != nil {
			cleanupFailed = true
		}
		if _, err := store.Pool.Exec(cleanup, `DELETE FROM source_build_tasks WHERE account_id=$1`, accountID); err != nil {
			cleanupFailed = true
		}
		if _, err := store.Pool.Exec(cleanup, `DELETE FROM source_artifacts WHERE account_id=$1`, accountID); err != nil {
			cleanupFailed = true
		}
		if _, err := store.Pool.Exec(cleanup, `DELETE FROM image_artifacts WHERE account_id=$1`, accountID); err != nil {
			cleanupFailed = true
		}
		if _, err := store.Pool.Exec(cleanup, `DELETE FROM accounts WHERE id=$1`, accountID); err != nil {
			cleanupFailed = true
		}
		if err := client.DeleteAccountRepository(cleanup, accountID, "agent"); err != nil {
			cleanupFailed = true
		}
		if err := client.DeleteAccountProject(cleanup, accountID); err != nil {
			cleanupFailed = true
		}
		var remaining int
		if err := store.Pool.QueryRow(cleanup, `SELECT
            (SELECT count(*) FROM accounts WHERE id=$1)+
            (SELECT count(*) FROM image_artifacts WHERE account_id=$1)+
            (SELECT count(*) FROM image_scan_tasks WHERE account_id=$1)+
            (SELECT count(*) FROM operations WHERE account_id=$1)`, accountID).Scan(&remaining); err != nil || remaining != 0 {
			cleanupFailed = true
		}
		if cleanupFailed {
			runErr = errors.Join(runErr, errors.New("temporary image scan smoke cleanup incomplete"))
		}
	}()
	_, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
        VALUES($1,$2,$2,'Image Scan Smoke')`, accountID, "image-scan-smoke-"+accountID)
	if err != nil {
		return errors.New("temporary account could not be created")
	}
	created = true
	image, err := resolver.Resolve(ctx, cfg.SourceReference, nil)
	if err != nil {
		return errors.New("approved source image could not be inspected")
	}
	if cfg.HarborSource {
		seedID, err := server.NewID()
		if err != nil {
			return err
		}
		pinned, err := client.MirrorAccountImage(ctx, accountID, seedID, image, resolver)
		if err != nil {
			return fmt.Errorf("private Harbor source could not be seeded: %w", err)
		}
		auth := authn.FromConfig(authn.AuthConfig{Username: "admin", Password: cfg.AdminPassword})
		image, err = resolver.Resolve(ctx, pinned, auth)
		if err != nil || image.RegistryHost != harbor.RegistryHost {
			return errors.New("private Harbor source could not be inspected")
		}
	}
	if cfg.ExpectFailure {
		if !cfg.Resident {
			return errors.New("terminal failure smoke requires the resident worker")
		}
		// Simulate an inspected source whose platform digest changed before copy.
		// The scanner must fail closed without writing Harbor approval evidence.
		image.PlatformDigest = "sha256:" + strings.Repeat("0", 64)
	}
	artifact, replay, err := store.SaveImageArtifact(ctx, accountID, "system:smoke", image)
	if err != nil || replay {
		return errors.New("source image did not enter the durable scan queue")
	}
	if cfg.ExpectFailure {
		wait, done := context.WithTimeout(ctx, 90*time.Second)
		defer done()
		for {
			var status, taskState, code, inspectState, inspectCode string
			err := store.Pool.QueryRow(wait, `SELECT a.scan_status,t.state,
                COALESCE(t.error_code,''),o.state,COALESCE(o.detail->>'errorCode','')
                FROM image_artifacts a JOIN image_scan_tasks t ON t.artifact_id=a.id
                JOIN operations o ON o.account_id=a.account_id AND o.kind='IMAGE_INSPECT'
                  AND o.detail->>'artifactId'=a.id::text
                WHERE a.id=$1 AND a.account_id=$2`, artifact.ID, accountID).Scan(
				&status, &taskState, &code, &inspectState, &inspectCode)
			if err != nil {
				return errors.New("terminal failure smoke task disappeared")
			}
			if taskState == "Failed" {
				if status != "Failed" || code != "IMAGE_MIRROR_FAILED" ||
					inspectState != "Failed" || inspectCode != code {
					return fmt.Errorf("terminal failure state mismatch: artifact=%s task=%s code=%s operation=%s/%s",
						status, taskState, code, inspectState, inspectCode)
				}
				items, err := store.ListImageArtifacts(wait, accountID)
				if err != nil || len(items) != 1 || items[0].ScanStatus != "Failed" ||
					items[0].ScanTaskState != "Failed" || items[0].ScanErrorCode != code {
					return errors.New("account image list did not show the terminal failure")
				}
				fmt.Println("IMAGE_SCAN_FAILURE_PASS:", code)
				return nil
			}
			select {
			case <-wait.Done():
				return errors.New("terminal failure smoke did not finish within 90 seconds")
			case <-time.After(2 * time.Second):
			}
		}
	}
	if cfg.Resident {
		wait, done := context.WithTimeout(ctx, 5*time.Minute)
		defer done()
		for {
			var state, taskState, code string
			err := store.Pool.QueryRow(wait, `SELECT a.scan_status,t.state,COALESCE(t.error_code,'')
                FROM image_artifacts a JOIN image_scan_tasks t ON t.artifact_id=a.id
                WHERE a.id=$1 AND a.account_id=$2`, artifact.ID, accountID).Scan(
				&state, &taskState, &code)
			if err != nil {
				return errors.New("resident scan task disappeared")
			}
			if (state == "Approved" || state == "Rejected") && taskState == "Succeeded" {
				break
			}
			if taskState == "Failed" {
				return fmt.Errorf("resident scan task failed: %v", code)
			}
			select {
			case <-wait.Done():
				return errors.New("resident scan task did not finish within five minutes")
			case <-time.After(3 * time.Second):
			}
		}
	} else {
		id, err := server.NewID()
		if err != nil {
			return err
		}
		worker := imagescan.Worker{Store: store, Harbor: client, Resolver: resolver, ID: id}
		processed, err := worker.RunOnce(ctx)
		if err != nil || !processed {
			return fmt.Errorf("scan worker did not complete one task: %v", err)
		}
	}
	var status, reference, taskState, policyVersion string
	var auditCount, highCount int
	err = store.Pool.QueryRow(ctx, `SELECT a.scan_status,a.harbor_reference,t.state,
	    COALESCE(a.scan_summary->>'policyVersion',''),
        (SELECT count(*) FROM operations WHERE account_id=$2 AND kind='IMAGE_SCAN'),
        COALESCE((a.scan_summary->'severities'->>'High')::int,0)
        FROM image_artifacts a JOIN image_scan_tasks t ON t.artifact_id=a.id
        WHERE a.id=$1 AND a.account_id=$2`, artifact.ID, accountID).Scan(
		&status, &reference, &taskState, &policyVersion, &auditCount, &highCount)
	project, _ := harbor.AccountProjectName(accountID)
	if err != nil || (cfg.ExpectStatus != "Any" && status != cfg.ExpectStatus) || taskState != "Succeeded" || auditCount != 1 ||
		policyVersion != harbor.PolicyVersion ||
		reference != harbor.RegistryHost+"/"+project+"/agent@"+image.PlatformDigest {
		return fmt.Errorf("scan queue/evidence mismatch: status=%s task=%s audits=%d", status, taskState, auditCount)
	}
	fmt.Printf("IMAGE_SCAN_CANDIDATE_RESULT: status=%s high=%d source=%s\n", status, highCount, cfg.SourceReference)
	if cfg.GCSmoke {
		if status != "Approved" || !cfg.Resident {
			return errors.New("image retention smoke requires a resident approved scan")
		}
		if err = runImageGCSmoke(ctx, store, client, accountID, artifact.ID); err != nil {
			return err
		}
	}
	if highCount > 0 {
		url := "https://" + harbor.RegistryHost + "/api/v2.0/projects/" + project +
			"/repositories/agent/artifacts/" + image.PlatformDigest + "/additions/vulnerabilities"
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if requestErr != nil {
			return requestErr
		}
		request.SetBasicAuth("admin", cfg.AdminPassword)
		request.Header.Set("X-Accept-Vulnerabilities", "application/vnd.security.vulnerability.report; version=1.1")
		response, requestErr := (&http.Client{Transport: transport, Timeout: 15 * time.Second}).Do(request)
		if requestErr != nil {
			return requestErr
		}
		report, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return errors.New("candidate vulnerability report is unavailable")
		}
		var details map[string]struct {
			Vulnerabilities []struct {
				ID         string `json:"id"`
				Package    string `json:"package"`
				Severity   string `json:"severity"`
				FixVersion string `json:"fix_version"`
			} `json:"vulnerabilities"`
		}
		if json.Unmarshal(report, &details) != nil {
			return errors.New("candidate vulnerability report is invalid")
		}
		shown := 0
		for _, reportSection := range details {
			for _, finding := range reportSection.Vulnerabilities {
				if !strings.EqualFold(finding.Severity, "High") {
					continue
				}
				fmt.Printf("HIGH_FINDING: %s package=%s fix=%s\n", finding.ID, finding.Package, finding.FixVersion)
				shown++
				if shown == 10 {
					break
				}
			}
		}
		if shown == 0 {
			fmt.Printf("HIGH_FINDING_REPORT_SHAPE: %s\n", string(report[:min(len(report), 500)]))
		}
	}
	if cfg.ExpectStatus == "Approved" {
		fmt.Println("IMAGE_SCAN_WORKER_PASS:", strings.TrimPrefix(reference, harbor.RegistryHost+"/"))
	}
	return nil
}

// runImageGCSmoke uses a disposable account. The two newer rows are synthetic
// policy sentinels; only the real scanned digest is ever deleted from Harbor.
func runImageGCSmoke(ctx context.Context, store *postgres.Store, client *harbor.Client,
	accountID, artifactID string) error {
	project, err := harbor.AccountProjectName(accountID)
	if err != nil {
		return err
	}
	sentinels := make([]string, 0, 2)
	for n := 1; n <= 2; n++ {
		id, idErr := server.NewID()
		if idErr != nil {
			return idErr
		}
		digest := "sha256:" + strings.Repeat(fmt.Sprint(n), 64)
		result, insertErr := store.Pool.Exec(ctx, `INSERT INTO image_artifacts(id,account_id,
            submitted_reference,pinned_reference,platform_digest,registry_host,configured_user,
            scan_status,harbor_reference,scan_report_sha256,scan_report_id,scanner_name,
            scan_completed_at,scan_summary)
            SELECT $1,account_id,$2,$3,$4,registry_host,configured_user,
                scan_status,$5,scan_report_sha256,scan_report_id,scanner_name,
                scan_completed_at,scan_summary
            FROM image_artifacts WHERE id=$6 AND account_id=$7`,
			id, fmt.Sprintf("registry-1.docker.io/library/busybox:gc-smoke-%d", n),
			"registry-1.docker.io/library/busybox@"+digest, digest,
			harbor.RegistryHost+"/"+project+"/agent@"+digest, artifactID, accountID)
		if insertErr != nil || result.RowsAffected() != 1 {
			return fmt.Errorf("retention sentinel insert failed: rows=%d error=%v", result.RowsAffected(), insertErr)
		}
		if _, err = store.Pool.Exec(ctx, `INSERT INTO image_scan_tasks(artifact_id,account_id,state)
            VALUES($1,$2,'Succeeded')`, id, accountID); err != nil {
			return err
		}
		sentinels = append(sentinels, id)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE image_artifacts
        SET created_at=clock_timestamp()-interval '31 days' WHERE id=$1 AND account_id=$2`,
		artifactID, accountID); err != nil {
		return err
	}
	sourceID, err := server.NewID()
	if err != nil {
		return err
	}
	buildID, err := server.NewID()
	if err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO source_artifacts(id,account_id,sha256,size_bytes)
        VALUES($1,$2,$3,1)`, sourceID, accountID, "sha256:"+strings.Repeat("3", 64)); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO source_build_tasks(id,artifact_id,account_id,state,result_image_artifact_id)
        VALUES($1,$2,$3,'Succeeded',$4)`, buildID, sourceID, accountID, artifactID); err != nil {
		return err
	}
	if _, found, err := store.ClaimExpiredImage(ctx, accountID); err != nil || found {
		return fmt.Errorf("retained build reference was reclaimable: found=%v error=%w", found, err)
	}
	if _, err = store.Pool.Exec(ctx, `DELETE FROM source_build_tasks WHERE id=$1`, buildID); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `DELETE FROM source_artifacts WHERE id=$1`, sourceID); err != nil {
		return err
	}
	item, found, err := store.ClaimExpiredImage(ctx, accountID)
	if err != nil || !found || item.ID != artifactID || item.AccountID != accountID {
		return fmt.Errorf("old unreferenced digest was not claimed: found=%v error=%w", found, err)
	}
	items, err := store.ListImageArtifacts(ctx, accountID)
	if err != nil || len(items) != 2 {
		return errors.New("claimed image remained in the student library")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE image_artifacts
        SET gc_started_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, artifactID); err != nil {
		return err
	}
	retry, found, err := store.ClaimExpiredImage(ctx, accountID)
	if err != nil || !found || retry.ID != artifactID {
		return fmt.Errorf("interrupted image deletion was not retryable: found=%v error=%w", found, err)
	}
	if err = client.DeleteAccountArtifact(ctx, accountID, "agent", item.Digest); err != nil {
		return err
	}
	if err = store.FinishExpiredImage(ctx, retry); err != nil {
		return err
	}
	if _, found, err := store.ClaimExpiredImage(ctx, accountID); err != nil || found {
		return fmt.Errorf("newest approved images were reclaimed: found=%v error=%w", found, err)
	}
	var artifacts, audits int
	if err = store.Pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM image_artifacts WHERE account_id=$1),
        (SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_RETENTION_DELETE')`,
		accountID).Scan(&artifacts, &audits); err != nil || artifacts != 2 || audits != 1 {
		return fmt.Errorf("image retention audit mismatch: artifacts=%d audits=%d error=%w", artifacts, audits, err)
	}
	fmt.Println("IMAGE_RETENTION_SMOKE_PASS: referenced protected, retry recovered, old digest deleted, newest two retained")
	retiredSourceID, err := server.NewID()
	if err != nil {
		return err
	}
	retiredBuildID, err := server.NewID()
	if err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO source_artifacts(id,account_id,sha256,size_bytes)
        VALUES($1,$2,$3,1)`, retiredSourceID, accountID,
		"sha256:"+strings.Repeat("4", 64)); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO source_build_tasks(id,artifact_id,account_id,state,result_image_artifact_id)
        VALUES($1,$2,$3,'Succeeded',$4)`, retiredBuildID, retiredSourceID, accountID, sentinels[0]); err != nil {
		return err
	}
	if err = store.RequestImageDeletion(ctx, accountID, "smoke:student", sentinels[0]); err != nil {
		return fmt.Errorf("student deletion request failed: %w", err)
	}
	retiredBuild, err := store.GetSourceBuild(ctx, accountID, retiredBuildID)
	if err != nil || retiredBuild.ResultImageArtifactID != "" || retiredBuild.ResultImageDeletedAt == nil ||
		retiredBuild.State != "Succeeded" {
		return fmt.Errorf("retained build did not mark its result removed: %+v error=%v", retiredBuild, err)
	}
	if err = store.RequestImageDeletion(ctx, accountID, "smoke:student", sentinels[0]); err != nil {
		return fmt.Errorf("student deletion replay failed: %w", err)
	}
	items, err = store.ListImageArtifacts(ctx, accountID)
	if err != nil || len(items) != 1 || items[0].ID != sentinels[1] {
		return errors.New("student deletion request did not hide the image")
	}
	requested, found, err := store.ClaimRequestedImage(ctx, accountID)
	if err != nil || !found || requested.ID != sentinels[0] {
		return fmt.Errorf("requested image was not claimed: found=%v error=%w", found, err)
	}
	if err = client.DeleteAccountArtifact(ctx, accountID, "agent", requested.Digest); err != nil {
		return err
	}
	if err = store.FinishExpiredImage(ctx, requested); err != nil {
		return err
	}
	var completedRequests int
	if err = store.Pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_USER_DELETE'),
        (SELECT count(*) FROM operations WHERE account_id=$1 AND kind='IMAGE_DELETE_REQUEST'
            AND state='Succeeded' AND detail ? 'deletedAt')`, accountID).Scan(&audits, &completedRequests); err != nil || audits != 1 || completedRequests != 1 {
		return fmt.Errorf("student image deletion audit mismatch: deleted=%d requests=%d error=%w",
			audits, completedRequests, err)
	}
	fmt.Println("IMAGE_USER_DELETE_SMOKE_PASS: replay, immediate hide, Harbor 404 and audit")
	return nil
}
