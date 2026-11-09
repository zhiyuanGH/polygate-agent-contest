package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

func (s *Store) GetImageScanReportPointer(ctx context.Context, accountID, artifactID string) (server.ImageScanReportPointer, error) {
	var item server.ImageScanReportPointer
	err := s.Pool.QueryRow(ctx, `SELECT harbor_reference,scan_report_id,scan_report_sha256,scan_status,
		scan_report_body,COALESCE(scan_report_body_sha256,'')
		FROM image_artifacts WHERE id=$1 AND account_id=$2 AND gc_state='Active'
		AND scan_status IN ('Approved','Rejected')`, artifactID, accountID).Scan(
		&item.HarborReference, &item.ReportID, &item.ReportSHA256, &item.ScanStatus,
		&item.ReportBody, &item.BodySHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, server.ErrNotFound
	}
	if err != nil {
		return item, storeError(err)
	}
	return item, nil
}

// ArchiveImageScanReport writes only for the same account, immutable image
// digest and Harbor scan ID checked when the report was fetched.
func (s *Store) ArchiveImageScanReport(ctx context.Context, accountID, artifactID,
	harborReference, reportID string, body []byte) error {
	if len(body) == 0 || len(body) > 8<<20 || !json.Valid(body) {
		return errors.New("invalid scan report body")
	}
	sum := sha256.Sum256(body)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	result, err := s.Pool.Exec(ctx, `UPDATE image_artifacts SET
		scan_report_body=$5,scan_report_body_sha256=$6
		WHERE id=$1 AND account_id=$2 AND harbor_reference=$3 AND scan_report_id=$4
		AND gc_state='Active' AND scan_status IN ('Approved','Rejected')
		AND scan_report_body IS NULL`, artifactID, accountID, harborReference, reportID, body, hash)
	if err != nil {
		return storeError(err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var archivedHash string
	err = s.Pool.QueryRow(ctx, `SELECT scan_report_body_sha256 FROM image_artifacts
		WHERE id=$1 AND account_id=$2 AND harbor_reference=$3 AND scan_report_id=$4
		AND gc_state='Active' AND scan_status IN ('Approved','Rejected')
		AND scan_report_body IS NOT NULL`, artifactID, accountID, harborReference, reportID).Scan(&archivedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrNotFound
	}
	if err != nil {
		return storeError(err)
	}
	if archivedHash == "" {
		return errors.New("scan report archive has no hash")
	}
	return nil
}

type UnarchivedScanReport struct {
	AccountID  string
	ArtifactID string
	server.ImageScanReportPointer
}

func (s *Store) ListUnarchivedScanReports(ctx context.Context, limit int) ([]UnarchivedScanReport, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid scan report batch size")
	}
	rows, err := s.Pool.Query(ctx, `SELECT account_id::text,id::text,harbor_reference,
		scan_report_id,scan_report_sha256,scan_status
		FROM image_artifacts WHERE gc_state='Active' AND scan_status IN ('Approved','Rejected')
		AND scan_report_body IS NULL ORDER BY created_at,id LIMIT $1`, limit)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]UnarchivedScanReport, 0)
	for rows.Next() {
		var item UnarchivedScanReport
		if err := rows.Scan(&item.AccountID, &item.ArtifactID, &item.HarborReference,
			&item.ReportID, &item.ReportSHA256, &item.ScanStatus); err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}
