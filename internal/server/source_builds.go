package server

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"polygate/internal/source"
)

func (h *Handler) sourceBuildStore(w http.ResponseWriter, r *http.Request, accountID string) (SourceBuildStore, bool) {
	store, ok := h.store.(SourceBuildStore)
	if !h.buildEnabled || h.buildRoot == "" || !ok {
		writeError(w, http.StatusServiceUnavailable, "BUILD_UNAVAILABLE", "Source builds are not available yet.")
		return nil, false
	}
	account, err := h.store.Account(r.Context(), accountID)
	if err != nil {
		writeStoreError(w, err)
		return nil, false
	}
	if !account.SourceBuildEnabled {
		writeError(w, http.StatusForbidden, "BUILD_NOT_ENABLED", "Source builds are not enabled for this account.")
		return nil, false
	}
	return store, true
}

func (h *Handler) listSourceBuilds(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.sourceBuildStore(w, r, p.AccountID)
	if !ok {
		return
	}
	items, err := store.ListSourceBuilds(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": items})
}

func (h *Handler) sourceBuildLog(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.sourceBuildStore(w, r, p.AccountID)
	if !ok {
		return
	}
	path, err := source.BuildLogPath(h.buildRoot, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Build not found.")
		return
	}
	task, err := store.GetSourceBuild(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 || offset > 64<<20 {
		writeError(w, http.StatusBadRequest, "INVALID_OFFSET", "Invalid build log offset.")
		return
	}
	pathInfo, err := os.Lstat(path)
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, map[string]any{"data": "", "nextOffset": 0, "state": task.State, "retained": false})
		return
	}
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0077 != 0 || pathInfo.Size() > 64<<20 {
		writeError(w, http.StatusServiceUnavailable, "BUILD_LOG_UNAVAILABLE", "Build log is temporarily unavailable.")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "BUILD_LOG_UNAVAILABLE", "Build log is temporarily unavailable.")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, info) || info.Size() > 64<<20 {
		writeError(w, http.StatusServiceUnavailable, "BUILD_LOG_UNAVAILABLE", "Build log is temporarily unavailable.")
		return
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	buf := make([]byte, min(int64(64<<10), info.Size()-offset))
	n, err := file.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		writeError(w, http.StatusServiceUnavailable, "BUILD_LOG_UNAVAILABLE", "Build log is temporarily unavailable.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"data": base64.StdEncoding.EncodeToString(buf[:n]),
		"nextOffset": offset + int64(n), "state": task.State, "retained": true})
}

func (h *Handler) submitSourceBuild(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.sourceBuildStore(w, r, p.AccountID)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, source.MaxArchiveBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Upload a ZIP source file.")
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "source" || !strings.HasSuffix(strings.ToLower(strings.TrimSpace(part.FileName())), ".zip") {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Upload a ZIP file in the source field.")
		return
	}
	f, err := os.CreateTemp("", "polygate-source-*.zip")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not inspect the source file.")
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	size, err := io.Copy(f, io.LimitReader(part, source.MaxArchiveBytes+1))
	if err != nil || size > source.MaxArchiveBytes {
		writeError(w, http.StatusBadRequest, "SOURCE_TOO_LARGE", "ZIP must be at most 100MiB.")
		return
	}
	kind := "dockerfile"
	next, nextErr := reader.NextPart()
	if nextErr == nil && next != nil {
		if next.FormName() != "template" || next.FileName() != "" {
			writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Only a template field may follow the ZIP file.")
			return
		}
		value, readErr := io.ReadAll(io.LimitReader(next, 33))
		if readErr != nil || len(value) > 32 {
			writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Invalid build template.")
			return
		}
		kind = string(value)
		next, nextErr = reader.NextPart()
	}
	if nextErr != io.EOF || next != nil {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Upload one ZIP and one optional template field.")
		return
	}
	if err = source.ValidateZIP(f, size); err != nil {
		if errors.Is(err, source.ErrInvalidArchive) {
			writeError(w, http.StatusBadRequest, "INVALID_SOURCE_ARCHIVE", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not inspect the source file.")
		}
		return
	}
	if err = source.CheckBuildInputs(f, size, kind); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BUILD_INPUTS", err.Error())
		return
	}
	archive := f
	archiveSize := size
	if kind != "dockerfile" {
		prepared, createErr := os.CreateTemp("", "polygate-template-*.zip")
		if createErr != nil {
			writeError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE", "Could not prepare the source template.")
			return
		}
		defer os.Remove(prepared.Name())
		defer prepared.Close()
		if err = source.WriteTemplatedZIP(prepared, f, size, kind); err != nil {
			writeError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE", "Could not prepare the source template.")
			return
		}
		info, statErr := prepared.Stat()
		if statErr != nil || info.Size() > source.MaxArchiveBytes {
			writeError(w, http.StatusBadRequest, "SOURCE_TOO_LARGE", "ZIP with template must be at most 100MiB.")
			return
		}
		archiveSize = info.Size()
		if err = source.ValidateZIP(prepared, archiveSize); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_SOURCE_ARCHIVE", "ZIP with template is invalid.")
			return
		}
		archive = prepared
	}
	id, err := NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save the source file.")
		return
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save the source file.")
		return
	}
	_, err = store.ReserveSourceBuild(r.Context(), p.AccountID, p.Subject, id, archiveSize)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	digest, err := source.SaveArchive(h.buildRoot, id, archive, archiveSize)
	if err != nil {
		if abandoned, abandonErr := store.AbandonSourceBuild(r.Context(), p.AccountID, id); abandoned && abandonErr == nil {
			if path, pathErr := source.ArchivePath(h.buildRoot, id); pathErr == nil {
				_ = os.Remove(path)
			}
		}
		writeError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE", "Source storage is temporarily unavailable.")
		return
	}
	task, err := store.ActivateSourceBuild(r.Context(), p.AccountID, id, digest)
	if err != nil {
		// A database timeout can happen after commit. Preserve the archive and
		// reservation so the recovery job can reconcile it without data loss.
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}
