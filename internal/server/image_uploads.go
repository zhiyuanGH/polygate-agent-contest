package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"polygate/internal/harbor"
	"polygate/internal/source"
)

func (h *Handler) imageUploadStore(w http.ResponseWriter, r *http.Request, accountID string) (ImageArtifactStore, bool) {
	store, ok := h.store.(ImageArtifactStore)
	if !ok || h.imageUploader == nil || h.buildRoot == "" || !h.imageDeployEnabled {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Image archive upload is unavailable.")
		return nil, false
	}
	account, err := h.store.Account(r.Context(), accountID)
	if err != nil {
		writeStoreError(w, err)
		return nil, false
	}
	if !account.ImageDeployEnabled {
		writeError(w, http.StatusForbidden, "IMAGE_DEPLOY_NOT_ENABLED", "Image deployment is not enabled for this account.")
		return nil, false
	}
	return store, true
}

func (h *Handler) createImageUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok = h.imageUploadStore(w, r, p.AccountID); !ok {
		return
	}
	var input struct {
		SizeBytes int64 `json:"sizeBytes"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		input.SizeBytes < 1 || input.SizeBytes > source.MaxImageArchiveBytes {
		writeError(w, http.StatusBadRequest, "INVALID_IMAGE_UPLOAD", "Upload one docker-save tar archive of at most 1 GiB.")
		return
	}
	id, err := NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not start image upload.")
		return
	}
	item := source.Upload{ID: id, Owner: p.AccountID, Size: input.SizeBytes, Template: "image-tar"}
	if err = source.CreateUpload(h.buildRoot, item); err != nil {
		if errors.Is(err, source.ErrUploadQuota) {
			writeError(w, http.StatusTooManyRequests, "UPLOAD_LIMIT_REACHED", "Finish an existing upload before starting another.")
		} else {
			writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Could not start image upload.")
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "chunkBytes": source.UploadChunkBytes})
}

func (h *Handler) putImageUploadChunk(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if _, ok = h.imageUploadStore(w, r, p.AccountID); !ok {
		return
	}
	item, dir, err := source.LoadUpload(h.buildRoot, r.PathValue("id"), p.AccountID)
	if err != nil || item.Template != "image-tar" {
		writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND", "Image upload not found or expired.")
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || item.ChunkSize(index) == 0 || r.ContentLength != item.ChunkSize(index) {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD_CHUNK", "Chunk index or size is invalid.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, source.UploadChunkBytes+1)
	if err = source.SaveUploadChunk(dir, item, index, r.Body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD_CHUNK", "Chunk differs from the expected size or existing content.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"received": index})
}

func (h *Handler) completeImageUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.imageUploadStore(w, r, p.AccountID)
	if !ok {
		return
	}
	item, dir, err := source.LoadUpload(h.buildRoot, r.PathValue("id"), p.AccountID)
	if err != nil || item.Template != "image-tar" {
		writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND", "Image upload not found or expired.")
		return
	}
	resultPath := filepath.Join(dir, "result.json")
	if saved, readErr := os.ReadFile(resultPath); readErr == nil {
		var artifact ImageArtifact
		if json.Unmarshal(saved, &artifact) == nil && artifact.ID != "" {
			writeJSON(w, http.StatusOK, artifact)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Image upload receipt is invalid.")
		return
	}
	select {
	case h.imageUploadSlots <- struct{}{}:
		defer func() { <-h.imageUploadSlots }()
	default:
		writeError(w, http.StatusTooManyRequests, "IMAGE_UPLOAD_BUSY", "Another image is being imported. Retry shortly.")
		return
	}
	f, err := source.AssembleUploadAt(dir, item, h.buildRoot)
	if err != nil {
		writeError(w, http.StatusConflict, "UPLOAD_INCOMPLETE", "Upload all archive chunks first.")
		return
	}
	path := f.Name()
	_ = f.Close()
	defer os.Remove(path)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	image, err := h.imageUploader(ctx, p.AccountID, item.ID, path)
	if err != nil {
		if errors.Is(err, harbor.ErrInvalidImageTar) {
			writeError(w, http.StatusBadRequest, "INVALID_IMAGE_ARCHIVE", "Upload one docker-save tar containing a linux/amd64 image.")
		} else {
			writeError(w, http.StatusServiceUnavailable, "IMAGE_IMPORT_FAILED", "The image registry could not import this archive. Retry shortly.")
		}
		return
	}
	artifact, _, err := store.SaveImageArtifact(ctx, p.AccountID, p.Subject, image)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record the uploaded image.")
		return
	}
	tmp, err := os.CreateTemp(dir, ".result-*")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Could not record the uploaded image.")
		return
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Could not record the uploaded image.")
		return
	}
	if err = tmp.Close(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Could not record the uploaded image.")
		return
	}
	if err = os.Link(tmp.Name(), resultPath); err != nil && !os.IsExist(err) {
		writeError(w, http.StatusServiceUnavailable, "IMAGE_UPLOAD_UNAVAILABLE", "Could not record the uploaded image.")
		return
	}
	for i := 0; i < item.ChunkCount(); i++ {
		_ = os.Remove(filepath.Join(dir, strconv.FormatInt(int64(i/10), 10)+strconv.Itoa(i%10)+".part"))
	}
	writeJSON(w, http.StatusCreated, artifact)
}
