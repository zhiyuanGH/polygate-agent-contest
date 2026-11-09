package server

import (
	"context"
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
	if !ok || h.imageReportReader == nil {
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	report, err := h.imageReportReader(ctx, item.HarborReference, item.ReportID, item.ReportSHA256)
	if err != nil {
		log.Printf("image scan report read failed artifact=%s: %v", id, err)
		writeError(w, http.StatusServiceUnavailable, "SCAN_REPORT_UNAVAILABLE", "The saved scan report is temporarily unavailable.")
		return
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
