package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"polygate/internal/deploy"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func TestImageScanTasksLeaseRecoveryAndOwnership(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accountID, _ := server.NewID()
	if _, err = store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
        VALUES($1,$2,$2,'Scan Queue Test')`, accountID, "scan-queue-"+accountID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM operations WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM image_artifacts WHERE account_id=$1`, accountID)
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM accounts WHERE id=$1`, accountID)
	}()

	add := func(letter string) server.ImageArtifact {
		manifest := "sha256:" + strings.Repeat(letter, 64)
		image := registry.Image{OS: "linux", Architecture: "amd64", RegistryHost: "registry.example",
			SubmittedReference: "registry.example/team/agent:v1",
			PinnedReference:    "registry.example/team/agent@" + manifest,
			ManifestDigest:     manifest, PlatformDigest: manifest}
		artifact, replay, err := store.SaveImageArtifact(ctx, accountID, "student", image)
		if err != nil || replay {
			t.Fatalf("save image: replay=%v err=%v", replay, err)
		}
		if again, replay, err := store.SaveImageArtifact(ctx, accountID, "student", image); err != nil ||
			!replay || again.ID != artifact.ID {
			t.Fatalf("duplicate inspection created a second task: %v %v", replay, err)
		}
		return artifact
	}
	first, second := add("a"), add("b")
	var taskCount int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM image_scan_tasks WHERE account_id=$1`,
		accountID).Scan(&taskCount); err != nil || taskCount != 2 {
		t.Fatalf("queue must contain two tasks, got %d: %v", taskCount, err)
	}

	var wg sync.WaitGroup
	claims := make([]ImageScanTask, 2)
	claimErrs := make([]error, 2)
	for i := range claims {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var ok bool
			claims[i], ok, claimErrs[i] = store.ClaimNextImageScan(ctx,
				[]string{"worker-a", "worker-b"}[i], time.Minute)
			if claimErrs[i] == nil && !ok {
				claimErrs[i] = errors.New("no task claimed")
			}
		}(i)
	}
	wg.Wait()
	if claimErrs[0] != nil || claimErrs[1] != nil ||
		claims[0].ArtifactID == claims[1].ArtifactID ||
		claims[0].AccountID != accountID || claims[1].AccountID != accountID {
		t.Fatalf("concurrent claims: %+v %+v, %v %v", claims[0], claims[1], claimErrs[0], claimErrs[1])
	}
	if err = store.CompleteImageScanTask(ctx, claims[0].ArtifactID, "worker-b"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("wrong worker completed scan: %v", err)
	}
	if err = store.CompleteImageScanTask(ctx, claims[0].ArtifactID, "worker-a"); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("scan completed without evidence: %v", err)
	}
	if err = store.RetryImageScan(ctx, claims[0].ArtifactID, "worker-b", "SCAN_TIMEOUT", 0); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("wrong worker retried scan: %v", err)
	}
	if err = store.RetryImageScan(ctx, claims[0].ArtifactID, "worker-a", "SCAN_TIMEOUT", 0); err != nil {
		t.Fatal(err)
	}
	if err = store.FailImageScan(ctx, claims[1].ArtifactID, "worker-b", "SOURCE_DENIED"); err != nil {
		t.Fatal(err)
	}
	var failedStatus, failedTask, failedCode, inspectState, inspectCode string
	err = store.Pool.QueryRow(ctx, `SELECT a.scan_status,t.state,t.error_code,
        o.state,o.detail->>'errorCode' FROM image_artifacts a
        JOIN image_scan_tasks t ON t.artifact_id=a.id
        JOIN operations o ON o.account_id=a.account_id AND o.kind='IMAGE_INSPECT'
          AND o.detail->>'artifactId'=a.id::text
        WHERE a.id=$1`, claims[1].ArtifactID).Scan(&failedStatus, &failedTask,
		&failedCode, &inspectState, &inspectCode)
	if err != nil || failedStatus != "Failed" || failedTask != "Failed" ||
		failedCode != "SOURCE_DENIED" || inspectState != "Failed" || inspectCode != "SOURCE_DENIED" {
		t.Fatalf("terminal failure not atomic: artifact=%s task=%s code=%s operation=%s/%s err=%v",
			failedStatus, failedTask, failedCode, inspectState, inspectCode, err)
	}
	items, err := store.ListImageArtifacts(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	foundFailed := false
	for _, item := range items {
		if item.ID == claims[1].ArtifactID {
			foundFailed = item.ScanStatus == "Failed" && item.ScanTaskState == "Failed" &&
				item.ScanErrorCode == "SOURCE_DENIED"
		}
	}
	if !foundFailed {
		t.Fatal("account image list hid the terminal scan failure")
	}
	claimed, ok, err := store.ClaimNextImageScan(ctx, "worker-c", time.Minute)
	if err != nil || !ok || claimed.ArtifactID != claims[0].ArtifactID || claimed.Attempt != 2 {
		t.Fatalf("retry claim: %+v %v %v", claimed, ok, err)
	}

	// Simulate a crash after evidence was committed but before the lease was
	// acknowledged. The next claim reconciles it without rescanning.
	report := []byte(`{"vulnerabilities":[]}`)
	sum := sha256.Sum256(report)
	evidence := harbor.Evidence{Digest: claimed.PlatformDigest, ReportID: "report-1",
		ReportSHA256: "sha256:" + hex.EncodeToString(sum[:]), ScannerName: "Trivy",
		ScannedAt: time.Now().UTC(), Report: report, Severities: map[string]int{}}
	ref := "harbor.polygate-registry.svc.cluster.local/pg-u-" +
		strings.ReplaceAll(accountID, "-", "") + "/agent@" + claimed.PlatformDigest
	if decision, _, err := store.recordImageScanDirect(ctx, accountID, claimed.ArtifactID, ref, evidence); err != nil || decision != "Approved" {
		t.Fatalf("record scan evidence: %s %v", decision, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE image_scan_tasks SET lease_until=now()-interval '1 second'
        WHERE artifact_id=$1`, claimed.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = store.ClaimNextImageScan(ctx, "worker-d", time.Minute); err != nil || ok {
		t.Fatalf("completed evidence should reconcile, ok=%v err=%v", ok, err)
	}
	items, err = store.ListImageArtifacts(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	foundApproved := false
	for _, item := range items {
		if item.ID == claimed.ArtifactID {
			foundApproved = item.ScanStatus == "Approved" && item.ScanSummary != nil &&
				item.ScanSummary.Total == 0 && item.ScanSummary.PolicyVersion == harbor.PolicyVersion
		}
	}
	if !foundApproved {
		t.Fatal("account image list omitted approved scan summary")
	}
	var succeeded, failed int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='Succeeded'),
        count(*) FILTER (WHERE state='Failed') FROM image_scan_tasks WHERE account_id=$1`,
		accountID).Scan(&succeeded, &failed); err != nil || succeeded != 1 || failed != 1 {
		t.Fatalf("terminal queue states: succeeded=%d failed=%d err=%v", succeeded, failed, err)
	}
	third := add("c")
	leased, ok, err := store.ClaimNextImageScan(ctx, "worker-e", time.Minute)
	if err != nil || !ok || leased.ArtifactID != third.ID {
		t.Fatalf("third image claim: %+v %v %v", leased, ok, err)
	}
	thirdEvidence := evidence
	thirdEvidence.Digest = leased.PlatformDigest
	thirdRef := "harbor.polygate-registry.svc.cluster.local/pg-u-" +
		strings.ReplaceAll(accountID, "-", "") + "/agent@" + leased.PlatformDigest
	if _, _, err = store.RecordLeasedImageScan(ctx, accountID, third.ID,
		"worker-other", thirdRef, thirdEvidence); !errors.Is(err, deploy.ErrLeaseLost) {
		t.Fatalf("stale worker wrote scan evidence: %v", err)
	}
	if decision, replay, err := store.RecordLeasedImageScan(ctx, accountID, third.ID,
		"worker-e", thirdRef, thirdEvidence); err != nil || replay || decision != "Approved" {
		t.Fatalf("leased scan was not atomic: %s %v %v", decision, replay, err)
	}
	var taskState string
	if err = store.Pool.QueryRow(ctx, `SELECT state FROM image_scan_tasks WHERE artifact_id=$1`,
		third.ID).Scan(&taskState); err != nil || taskState != "Succeeded" {
		t.Fatalf("scan evidence committed without task acknowledgement: %q %v", taskState, err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT state FROM operations
        WHERE account_id=$1 AND kind='IMAGE_INSPECT' AND detail->>'artifactId'=$2`,
		accountID, third.ID).Scan(&inspectState); err != nil || inspectState != "Approved" {
		t.Fatalf("successful inspection operation stayed pending: %q %v", inspectState, err)
	}
	fourth := add("d")
	leased, ok, err = store.ClaimNextImageScan(ctx, "worker-f", time.Minute)
	if err != nil || !ok || leased.ArtifactID != fourth.ID {
		t.Fatalf("fourth image claim: %+v %v %v", leased, ok, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE image_scan_tasks SET attempt=8,
        lease_until=now()-interval '1 second' WHERE artifact_id=$1`, fourth.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = store.ClaimNextImageScan(ctx, "worker-g", time.Minute); err != nil || ok {
		t.Fatalf("exhausted scan should fail, ok=%v err=%v", ok, err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT state FROM image_scan_tasks WHERE artifact_id=$1`,
		fourth.ID).Scan(&taskState); err != nil || taskState != "Failed" {
		t.Fatalf("expired exhausted lease was not failed: %q %v", taskState, err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT scan_status FROM image_artifacts WHERE id=$1`,
		fourth.ID).Scan(&failedStatus); err != nil || failedStatus != "Failed" {
		t.Fatalf("exhausted scan artifact stayed pending: %q %v", failedStatus, err)
	}
	if first.ID == second.ID {
		t.Fatal("distinct images received the same artifact ID")
	}
}
