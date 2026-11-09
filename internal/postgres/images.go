package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/jackc/pgx/v5"
	"polygate/internal/deploy"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const maxImageArtifactsPerAccount = 100

func validateResolvedImage(image registry.Image) error {
	if image.OS != "linux" || image.Architecture != "amd64" ||
		len(image.SubmittedReference) == 0 || len(image.SubmittedReference) > 512 ||
		len(image.PinnedReference) == 0 || len(image.PinnedReference) > 600 ||
		len(image.ConfiguredUser) > 256 || !imageDigestPattern.MatchString(image.ManifestDigest) ||
		!imageDigestPattern.MatchString(image.PlatformDigest) {
		return errors.New("resolved image metadata is invalid")
	}
	ref, err := name.ParseReference(image.PinnedReference, name.StrictValidation)
	if err != nil || ref.Context().RegistryStr() != image.RegistryHost ||
		!strings.HasSuffix(image.PinnedReference, "@"+image.ManifestDigest) {
		return errors.New("resolved image reference does not match its registry and digest")
	}
	return nil
}

func scanImage(row rowScanner) (server.ImageArtifact, error) {
	var artifact server.ImageArtifact
	err := row.Scan(&artifact.ID, &artifact.SubmittedReference, &artifact.PinnedReference,
		&artifact.PlatformDigest, &artifact.RegistryHost, &artifact.ConfiguredUser,
		&artifact.ScanStatus, &artifact.CreatedAt)
	return artifact, err
}

func scanImageWithTask(row rowScanner) (server.ImageArtifact, error) {
	var artifact server.ImageArtifact
	var summary []byte
	err := row.Scan(&artifact.ID, &artifact.SubmittedReference, &artifact.PinnedReference,
		&artifact.PlatformDigest, &artifact.RegistryHost, &artifact.ConfiguredUser,
		&artifact.ScanStatus, &artifact.CreatedAt, &artifact.ScanTaskState,
		&artifact.ScanErrorCode, &summary)
	if err == nil && len(summary) > 0 {
		var parsed server.ImageScanSummary
		err = json.Unmarshal(summary, &parsed)
		if err == nil {
			// Keep the student response limited to known, aggregate severity counts.
			counts := make(map[string]int)
			for severity, n := range parsed.Severities {
				if n <= 0 || n > parsed.Total {
					continue
				}
				var label string
				switch strings.ToLower(severity) {
				case "critical":
					label = "Critical"
				case "high":
					label = "High"
				case "unknown":
					label = "Unknown"
				case "medium":
					label = "Medium"
				case "low":
					label = "Low"
				case "negligible":
					label = "Negligible"
				case "none":
					label = "None"
				}
				if label != "" && counts[label] <= parsed.Total-n {
					counts[label] += n
				}
			}
			parsed.Severities = counts
			artifact.ScanSummary = &parsed
		}
	}
	return artifact, err
}

const imageColumns = `id::text,submitted_reference,pinned_reference,platform_digest,
    registry_host,configured_user,scan_status,created_at`

const imageListColumns = `image_artifacts.id::text,image_artifacts.submitted_reference,
    image_artifacts.pinned_reference,image_artifacts.platform_digest,
    image_artifacts.registry_host,image_artifacts.configured_user,
    image_artifacts.scan_status,image_artifacts.created_at`

// SaveImageArtifact records a resolved digest under its account. The returned
// artifact remains PendingScan and cannot be submitted to the workload queue.
func (s *Store) SaveImageArtifact(ctx context.Context, accountID, subject string,
	image registry.Image) (server.ImageArtifact, bool, error) {
	if err := validateResolvedImage(image); err != nil {
		return server.ImageArtifact{}, false, err
	}
	artifactID, err := server.NewID()
	if err != nil {
		return server.ImageArtifact{}, false, err
	}
	operationID, err := server.NewID()
	if err != nil {
		return server.ImageArtifact{}, false, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled); err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	if !enabled {
		return server.ImageArtifact{}, false, server.ErrDisabled
	}
	var deleting bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_artifacts
        WHERE account_id=$1 AND platform_digest=$2 AND gc_state<>'Active')`,
		accountID, image.PlatformDigest).Scan(&deleting); err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	if deleting {
		return server.ImageArtifact{}, false, &domain.Error{Code: "IMAGE_CLEANUP_IN_PROGRESS",
			Message: "This digest is being removed. Retry inspection shortly."}
	}
	// The account lock serializes artifact admission for this tenant. Existing
	// digests remain replayable after the limit is reached.
	previous, err := scanImage(tx.QueryRow(ctx, `SELECT `+imageColumns+`
		FROM image_artifacts WHERE account_id=$1 AND pinned_reference=$2 AND gc_state='Active'`, accountID, image.PinnedReference))
	if err == nil {
		return previous, true, storeError(tx.Commit(ctx))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return server.ImageArtifact{}, false, storeError(err)
	}
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM image_artifacts WHERE account_id=$1 AND gc_state='Active'`, accountID).Scan(&count); err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	if count >= maxImageArtifactsPerAccount {
		return server.ImageArtifact{}, false, &domain.Error{Code: "IMAGE_ARTIFACT_LIMIT", Field: "reference",
			Message: "This account has reached its image inspection limit.", Current: count,
			Allowed: maxImageArtifactsPerAccount, Requested: 1}
	}
	artifact, err := scanImage(tx.QueryRow(ctx, `INSERT INTO image_artifacts(id,account_id,submitted_reference,
        pinned_reference,platform_digest,registry_host,configured_user)
        VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (account_id,pinned_reference) DO NOTHING
        RETURNING `+imageColumns, artifactID, accountID, image.SubmittedReference,
		image.PinnedReference, image.PlatformDigest, image.RegistryHost, image.ConfiguredUser))
	if errors.Is(err, pgx.ErrNoRows) {
		artifact, err = scanImage(tx.QueryRow(ctx, `SELECT `+imageColumns+`
			FROM image_artifacts WHERE account_id=$1 AND pinned_reference=$2 AND gc_state='Active'`, accountID, image.PinnedReference))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return server.ImageArtifact{}, false, &domain.Error{Code: "IMAGE_CLEANUP_IN_PROGRESS",
					Message: "This digest is being removed. Retry inspection shortly."}
			}
			return server.ImageArtifact{}, false, storeError(err)
		}
		return artifact, true, storeError(tx.Commit(ctx))
	}
	if err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO image_scan_tasks(artifact_id,account_id)
        VALUES($1,$2)`, artifact.ID, accountID)
	if err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	detail, err := json.Marshal(map[string]string{"artifactId": artifact.ID,
		"submittedReference": artifact.SubmittedReference, "pinnedReference": artifact.PinnedReference})
	if err != nil {
		return server.ImageArtifact{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,'IMAGE_INSPECT','PendingScan',$4::jsonb)`, operationID, accountID, subject, detail)
	if err != nil {
		return server.ImageArtifact{}, false, storeError(err)
	}
	return artifact, false, storeError(tx.Commit(ctx))
}

func (s *Store) ListImageArtifacts(ctx context.Context, accountID string) ([]server.ImageArtifact, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+imageListColumns+`,
		COALESCE(t.state,''),COALESCE(t.error_code,''),image_artifacts.scan_summary FROM image_artifacts
        LEFT JOIN image_scan_tasks t ON t.artifact_id=image_artifacts.id
            AND t.account_id=image_artifacts.account_id
		WHERE image_artifacts.account_id=$1 AND image_artifacts.gc_state='Active'
        ORDER BY image_artifacts.created_at DESC,image_artifacts.id DESC`, accountID)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.ImageArtifact, 0)
	for rows.Next() {
		artifact, err := scanImageWithTask(rows)
		if err != nil {
			return nil, storeError(err)
		}
		items = append(items, artifact)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError(err)
	}
	return items, nil
}

// recordImageScanDirect supports internal evidence validation and recovery
// tests. Production scanner code must use RecordLeasedImageScan.
func (s *Store) recordImageScanDirect(ctx context.Context, accountID, artifactID,
	harborReference string, evidence harbor.Evidence) (string, bool, error) {
	return s.recordImageScan(ctx, accountID, artifactID, "", harborReference, evidence)
}

// RecordLeasedImageScan is the scanner worker's only write path. It holds the
// task lease lock while recording evidence and acknowledging the task in one
// transaction, so a stale worker cannot approve an image after losing its lease.
func (s *Store) RecordLeasedImageScan(ctx context.Context, accountID, artifactID,
	workerID, harborReference string, evidence harbor.Evidence) (string, bool, error) {
	if workerID == "" {
		return "", false, errors.New("image scan worker ID is required")
	}
	return s.recordImageScan(ctx, accountID, artifactID, workerID, harborReference, evidence)
}

func (s *Store) recordImageScan(ctx context.Context, accountID, artifactID,
	workerID, harborReference string, evidence harbor.Evidence) (string, bool, error) {
	decision, err := evidence.Decision()
	if err != nil || len(evidence.Report) > 8<<20 ||
		evidence.ScannedAt.After(time.Now().Add(5*time.Minute)) ||
		evidence.ScannedAt.Before(time.Now().Add(-24*time.Hour)) {
		return "", false, errors.New("scan evidence is invalid or stale")
	}
	ref, err := name.NewDigest(harborReference, name.StrictValidation)
	project := "pg-u-" + strings.ReplaceAll(accountID, "-", "")
	if err != nil || ref.Context().RegistryStr() != "harbor.polygate-registry.svc.cluster.local" ||
		!strings.HasPrefix(ref.Context().RepositoryStr(), project+"/") ||
		ref.DigestStr() != evidence.Digest {
		return "", false, errors.New("Harbor reference is not bound to the account and scan digest")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", false, storeError(err)
	}
	defer tx.Rollback(ctx)
	if workerID != "" {
		var leasedID string
		err = tx.QueryRow(ctx, `SELECT artifact_id::text FROM image_scan_tasks
            WHERE artifact_id=$1 AND account_id=$2 AND state='Leased'
              AND locked_by=$3 AND lease_until>clock_timestamp() FOR UPDATE`,
			artifactID, accountID, workerID).Scan(&leasedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, deploy.ErrLeaseLost
		}
		if err != nil {
			return "", false, storeError(err)
		}
	}
	var status, platformDigest, oldHarbor, oldReport string
	err = tx.QueryRow(ctx, `SELECT scan_status,platform_digest,
        COALESCE(harbor_reference,''),COALESCE(scan_report_sha256,'')
        FROM image_artifacts WHERE id=$1 AND account_id=$2 FOR UPDATE`,
		artifactID, accountID).Scan(&status, &platformDigest, &oldHarbor, &oldReport)
	if err != nil {
		return "", false, storeError(err)
	}
	if platformDigest != evidence.Digest {
		return "", false, errors.New("Harbor manifest differs from the inspected platform digest")
	}
	if status != "PendingScan" {
		if status == decision && oldHarbor == harborReference && oldReport == evidence.ReportSHA256 {
			if workerID != "" {
				if err = completeLeasedImageScanTx(ctx, tx, artifactID, workerID); err != nil {
					return "", false, err
				}
			}
			return status, true, storeError(tx.Commit(ctx))
		}
		return "", false, &domain.Error{Code: "IMAGE_SCAN_CONFLICT",
			Message: "This image already has a different scan result."}
	}
	summary, err := json.Marshal(map[string]any{"total": evidence.Total,
		"fixable": evidence.Fixable, "severities": evidence.Severities,
		"policyVersion": harbor.PolicyVersion})
	if err != nil {
		return "", false, err
	}
	_, err = tx.Exec(ctx, `UPDATE image_artifacts SET scan_status=$3,
        harbor_reference=$4,scan_report_sha256=$5,scan_report_id=$6,
        scanner_name=$7,scan_completed_at=$8,scan_summary=$9::jsonb
        WHERE id=$1 AND account_id=$2 AND scan_status='PendingScan'`,
		artifactID, accountID, decision, harborReference, evidence.ReportSHA256,
		evidence.ReportID, evidence.ScannerName, evidence.ScannedAt, summary)
	if err != nil {
		return "", false, storeError(err)
	}
	opID, err := server.NewID()
	if err != nil {
		return "", false, err
	}
	detail, err := json.Marshal(map[string]any{"artifactId": artifactID,
		"harborReference": harborReference, "reportSHA256": evidence.ReportSHA256,
		"scanner": evidence.ScannerName, "summary": json.RawMessage(summary)})
	if err != nil {
		return "", false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,'system:harbor-scanner','IMAGE_SCAN',$3,$4::jsonb)`,
		opID, accountID, decision, detail)
	if err != nil {
		return "", false, storeError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE operations SET state=$3
        WHERE account_id=$1 AND kind='IMAGE_INSPECT' AND state='PendingScan'
          AND detail->>'artifactId'=$2`, accountID, artifactID, decision)
	if err != nil {
		return "", false, storeError(err)
	}
	if workerID != "" {
		if err = completeLeasedImageScanTx(ctx, tx, artifactID, workerID); err != nil {
			return "", false, err
		}
	}
	return decision, false, storeError(tx.Commit(ctx))
}

func completeLeasedImageScanTx(ctx context.Context, tx pgx.Tx, artifactID, workerID string) error {
	result, err := tx.Exec(ctx, `UPDATE image_scan_tasks SET state='Succeeded',
        locked_by=NULL,lease_until=NULL,error_code=NULL,updated_at=now()
        WHERE artifact_id=$1 AND state='Leased' AND locked_by=$2 AND lease_until>clock_timestamp()`,
		artifactID, workerID)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() != 1 {
		return deploy.ErrLeaseLost
	}
	return nil
}
