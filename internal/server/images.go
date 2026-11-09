package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(ImageDeletionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_DELETION_DISABLED", "Image deletion is unavailable.")
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_ID", "Select one image to delete.")
		return
	}
	if err := store.RequestImageDeletion(r.Context(), p.AccountID, p.Subject, id); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "Requested"})
}

func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(ImageArtifactStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_INSPECTION_DISABLED", "Image inspection is not available yet.")
		return
	}
	items, err := store.ListImageArtifacts(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) inspectImage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, available := h.store.(ImageArtifactStore)
	if !available || h.imageInspector == nil {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_INSPECTION_DISABLED", "Image inspection is not available yet.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	defer r.Body.Close()
	var request struct {
		Reference string `json:"reference"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Reference == "" || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_REQUEST", "Provide one image reference.")
		return
	}
	select {
	case h.imageInspectSlots <- struct{}{}:
		defer func() { <-h.imageInspectSlots }()
	default:
		writeError(w, http.StatusTooManyRequests, "IMAGE_INSPECTION_BUSY", "Image inspection is busy. Please retry shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	image, err := h.imageInspector(ctx, request.Reference)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "IMAGE_REGISTRY_TIMEOUT", "The image registry did not respond in time.")
			return
		}
		// Registry errors can contain signed blob URLs or private auth details.
		writeError(w, http.StatusBadRequest, "IMAGE_RESOLVE_FAILED", "The image could not be inspected from an approved registry.")
		return
	}
	artifact, replay, err := store.SaveImageArtifact(ctx, p.AccountID, p.Subject, image)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, artifact)
}

func (h *Handler) deployApprovedImage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	deployer, available := h.store.(ApprovedImageDeployStore)
	if !h.imageDeployEnabled || !available {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_DEPLOYMENT_DISABLED",
			"Approved image deployment is not available yet.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	defer r.Body.Close()
	var request struct {
		ArtifactID string `json:"artifactId"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ArtifactID == "" ||
		decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_DEPLOY_REQUEST",
			"Select one approved image.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY",
			"Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	task, replay, err := deployer.QueueApprovedImageDeploy(r.Context(), p.AccountID,
		p.Subject, r.PathValue("id"), key, request.ArtifactID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *Handler) updateApprovedImage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	updater, available := h.store.(ApprovedImageUpdateStore)
	if !h.imageDeployEnabled || !available {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPDATE_DISABLED",
			"Approved image updates are not available yet.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	defer r.Body.Close()
	var request struct {
		ArtifactID string `json:"artifactId"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ArtifactID == "" ||
		decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_DEPLOY_REQUEST", "Select one approved image.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY",
			"Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	task, replay, err := updater.QueueApprovedImageUpdate(r.Context(), p.AccountID,
		p.Subject, r.PathValue("id"), key, request.ArtifactID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *Handler) rollbackRevision(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	rollback, available := h.store.(RevisionRollbackStore)
	if !h.imageDeployEnabled || !available {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_ROLLBACK_DISABLED", "Image rollback is not available yet.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	defer r.Body.Close()
	var request struct {
		RevisionID string `json:"revisionId"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.RevisionID == "" ||
		decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_ROLLBACK_REQUEST", "Select one previous revision.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	task, replay, err := rollback.QueueApprovedRevisionRollback(r.Context(), p.AccountID,
		p.Subject, r.PathValue("id"), key, request.RevisionID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}
