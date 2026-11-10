package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrImageScanNotRetryable   = errors.New("only a failed image inspection can be retried")
	ErrImageCleanupInProgress  = errors.New("image cleanup is in progress")
	ErrImageScanRetryKeyReused = errors.New("image scan retry key was used for another image")
)

// ImageScanRetryStore retries the existing immutable artifact, without resolving
// a new reference or granting approval. Authorization and queue changes are
// committed together by the store.
type ImageScanRetryStore interface {
	QueueImageScanRetry(context.Context, string, string, string, string) (ImageArtifact, bool, error)
}

func (h *Handler) retryImageScan(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(ImageScanRetryStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_SCAN_RETRY_UNAVAILABLE", "Image inspection retry is unavailable.")
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_ID", "Select a failed image inspection to retry.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(body)) != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_SCAN_RETRY_REQUEST", "Retry a failed inspection without a request body.")
		return
	}
	artifact, replay, err := store.QueueImageScanRetry(r.Context(), p.AccountID, p.Subject, id.String(), key)
	switch {
	case errors.Is(err, ErrImageScanNotRetryable):
		writeError(w, http.StatusConflict, "IMAGE_SCAN_NOT_RETRYABLE", "Only a failed inspection can be retried. Refresh My images to see its current status.")
		return
	case errors.Is(err, ErrImageCleanupInProgress):
		writeError(w, http.StatusConflict, "IMAGE_CLEANUP_IN_PROGRESS", "This image is being removed and cannot be inspected again.")
		return
	case errors.Is(err, ErrImageScanRetryKeyReused):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "This request key was already used to retry another image.")
		return
	case err != nil:
		writeStoreError(w, err)
		return
	}
	status := http.StatusAccepted
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, artifact)
}
