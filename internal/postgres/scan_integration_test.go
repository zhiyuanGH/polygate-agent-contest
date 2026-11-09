package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func TestImageScanEvidenceIsAccountBoundAndAtomic(t *testing.T) {
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
	aID, _ := server.NewID()
	bID, _ := server.NewID()
	for _, id := range []string{aID, bID} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
            VALUES($1,$2,$2,'Scan Test')`, id, "scan-test-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM image_artifacts WHERE account_id=ANY($1::uuid[])`, []string{aID, bID})
		_, _ = store.Pool.Exec(cleanCtx, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, []string{aID, bID})
	}()
	makeImage := func(letter string) registry.Image {
		manifest := "sha256:" + strings.Repeat(letter, 64)
		platform := "sha256:" + strings.Repeat("b", 64)
		if letter != "a" {
			platform = "sha256:" + strings.Repeat("d", 64)
		}
		return registry.Image{OS: "linux", Architecture: "amd64", RegistryHost: "registry.example",
			SubmittedReference: "registry.example/team/agent:v1",
			PinnedReference:    "registry.example/team/agent@" + manifest,
			ManifestDigest:     manifest, PlatformDigest: platform}
	}
	first, _, err := store.SaveImageArtifact(ctx, aID, "student-a", makeImage("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.SaveImageArtifact(ctx, aID, "student-a", makeImage("c"))
	if err != nil {
		t.Fatal(err)
	}
	makeEvidence := func(digest string, severities map[string]int, total int) harbor.Evidence {
		report := []byte(`{"vulnerabilities":[]}`)
		sum := sha256.Sum256(report)
		return harbor.Evidence{Digest: digest, ReportID: "scan-report-1",
			ReportSHA256: "sha256:" + hex.EncodeToString(sum[:]), ScannerName: "Trivy",
			ScannedAt: time.Now().UTC(), Total: total, Severities: severities, Report: report}
	}
	firstEvidence := makeEvidence(first.PlatformDigest, map[string]int{}, 0)
	ref := func(accountID, digest string) string {
		return "harbor.polygate-registry.svc.cluster.local/pg-u-" +
			strings.ReplaceAll(accountID, "-", "") + "/agent@" + digest
	}
	if _, _, err := store.recordImageScanDirect(ctx, bID, first.ID,
		ref(bID, firstEvidence.Digest), firstEvidence); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account scan update accepted: %v", err)
	}
	forged := firstEvidence
	forged.ReportSHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, _, err := store.recordImageScanDirect(ctx, aID, first.ID, ref(aID, forged.Digest), forged); err == nil {
		t.Fatal("forged report digest accepted")
	}
	wrong := makeEvidence("sha256:"+strings.Repeat("e", 64), map[string]int{}, 0)
	if _, _, err := store.recordImageScanDirect(ctx, aID, first.ID, ref(aID, wrong.Digest), wrong); err == nil {
		t.Fatal("different platform digest accepted")
	}
	decision, replay, err := store.recordImageScanDirect(ctx, aID, first.ID,
		ref(aID, firstEvidence.Digest), firstEvidence)
	if err != nil || replay || decision != "Approved" {
		t.Fatalf("clean scan: decision=%q replay=%v err=%v", decision, replay, err)
	}
	decision, replay, err = store.recordImageScanDirect(ctx, aID, first.ID,
		ref(aID, firstEvidence.Digest), firstEvidence)
	if err != nil || !replay || decision != "Approved" {
		t.Fatalf("idempotent scan: decision=%q replay=%v err=%v", decision, replay, err)
	}
	critical := makeEvidence(second.PlatformDigest, map[string]int{"Critical": 1}, 1)
	decision, replay, err = store.recordImageScanDirect(ctx, aID, second.ID, ref(aID, critical.Digest), critical)
	if err != nil || replay || decision != "Rejected" {
		t.Fatalf("critical severity scan: decision=%q replay=%v err=%v", decision, replay, err)
	}
	archived, err := store.GetImageScanReportPointer(ctx, aID, second.ID)
	if err != nil || string(archived.ReportBody) != string(critical.Report) ||
		archived.BodySHA256 != critical.ReportSHA256 {
		t.Fatalf("scan report was not archived with evidence: %+v %v", archived, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE image_artifacts SET
		scan_report_body=NULL,scan_report_body_sha256=NULL WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	backfillBody := []byte(`{"vulnerabilities":[{"id":"CVE-BACKFILL"}]}`)
	if err := store.ArchiveImageScanReport(ctx, bID, second.ID, ref(aID, critical.Digest),
		critical.ReportID, backfillBody); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("cross-account report archive accepted: %v", err)
	}
	if err := store.ArchiveImageScanReport(ctx, aID, second.ID, ref(aID, critical.Digest),
		critical.ReportID, backfillBody); err != nil {
		t.Fatal(err)
	}
	archived, err = store.GetImageScanReportPointer(ctx, aID, second.ID)
	if err != nil || string(archived.ReportBody) != string(backfillBody) ||
		archived.BodySHA256 == critical.ReportSHA256 {
		t.Fatalf("backfilled report mismatch: %+v %v", archived, err)
	}
	var approved, rejected, audits int
	if err := store.Pool.QueryRow(ctx, `SELECT
        count(*) FILTER (WHERE scan_status='Approved'),
        count(*) FILTER (WHERE scan_status='Rejected')
        FROM image_artifacts WHERE account_id=$1`, aID).Scan(&approved, &rejected); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM operations
        WHERE account_id=$1 AND kind='IMAGE_SCAN'`, aID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if approved != 1 || rejected != 1 || audits != 2 {
		t.Fatalf("state or audit mismatch: approved=%d rejected=%d audit=%d", approved, rejected, audits)
	}
	_, err = store.Pool.Exec(ctx, `UPDATE image_artifacts SET harbor_reference=NULL WHERE id=$1`, first.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("database allowed approval without Harbor evidence: %v", err)
	}
}
