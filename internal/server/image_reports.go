package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func (h *Handler) imageScanReport(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(ImageScanReportStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SCAN_REPORT_UNAVAILABLE", "Image scan reports are unavailable.")
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_ID", "Select an image to view its report.")
		return
	}
	item, err := store.GetImageScanReportPointer(r.Context(), p.AccountID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	report := item.ReportBody
	if len(report) > 0 {
		sum := sha256.Sum256(report)
		if len(report) > 8<<20 || !json.Valid(report) ||
			item.BodySHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
			log.Printf("image scan report archive invalid artifact=%s", id)
			writeError(w, http.StatusServiceUnavailable, "SCAN_REPORT_UNAVAILABLE", "The saved scan report is temporarily unavailable.")
			return
		}
	} else {
		if h.imageReportReader == nil {
			writeError(w, http.StatusServiceUnavailable, "SCAN_REPORT_UNAVAILABLE", "The saved scan report is temporarily unavailable.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		report, err = h.imageReportReader(ctx, item.HarborReference, item.ReportID, item.ReportSHA256)
		if err != nil {
			log.Printf("image scan report read failed artifact=%s: %v", id, err)
			writeError(w, http.StatusServiceUnavailable, "SCAN_REPORT_UNAVAILABLE", "The saved scan report is temporarily unavailable.")
			return
		}
		if archiver, ok := h.store.(ImageScanReportArchiver); ok {
			if err := archiver.ArchiveImageScanReport(ctx, p.AccountID, id,
				item.HarborReference, item.ReportID, report); err != nil {
				log.Printf("image scan report archive failed artifact=%s: %v", id, err)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=polygate-scan-"+id+".json")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(report)
}
