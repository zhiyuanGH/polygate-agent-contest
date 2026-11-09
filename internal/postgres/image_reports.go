package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

func (s *Store) GetImageScanReportPointer(ctx context.Context, accountID, artifactID string) (server.ImageScanReportPointer, error) {
	var item server.ImageScanReportPointer
	err := s.Pool.QueryRow(ctx, `SELECT harbor_reference,scan_report_id,scan_report_sha256,scan_status
		FROM image_artifacts WHERE id=$1 AND account_id=$2 AND gc_state='Active'
		AND scan_status IN ('Approved','Rejected')`, artifactID, accountID).Scan(
		&item.HarborReference, &item.ReportID, &item.ReportSHA256, &item.ScanStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, server.ErrNotFound
	}
	if err != nil {
		return item, storeError(err)
	}
	return item, nil
}
