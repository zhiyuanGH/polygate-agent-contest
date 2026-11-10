package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"polygate/internal/domain"
)

type AppStorageEditor interface {
	UpdateAppStorage(context.Context, string, string, string, bool, string, string) (App, error)
}

func (h *Handler) updateAppStorage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	editor, available := h.store.(AppStorageEditor)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "STORAGE_EDIT_UNAVAILABLE", "App storage editing is unavailable.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	defer r.Body.Close()
	var input struct {
		Persistent *bool  `json:"persistent"`
		VolumeID   string `json:"volumeId"`
		MountPath  string `json:"mountPath"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Persistent == nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_STORAGE_EDIT", "Send one storage settings object with an explicit persistent value.")
		return
	}
	if *input.Persistent && !h.volumeEnabled {
		writeError(w, http.StatusServiceUnavailable, "VOLUME_CATALOG_DISABLED", "Volume reservations are unavailable.")
		return
	}
	app, err := editor.UpdateAppStorage(r.Context(), p.AccountID, p.Subject,
		r.PathValue("id"), *input.Persistent, input.VolumeID, input.MountPath)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// UpdateAppStorage changes only the saved binding. It never creates, deletes,
// or changes a volume and cannot change a live workload. The development store
// has no cleanup evidence, so failed apps must be stopped before editing here.
func (s *MemoryStore) UpdateAppStorage(_ context.Context, accountID, subject, appID string,
	persistent bool, volumeID, mountPath string) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[accountID][appID]
	if !ok {
		return App{}, ErrNotFound
	}
	if app.Status == "Failed" {
		return App{}, &domain.Error{Code: "APP_CLEANUP_PENDING", Message: "Wait until workload cleanup is verified before changing storage."}
	}
	if app.Status != "Draft" && app.Status != "Stopped" {
		return App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its storage settings."}
	}
	updated := app.Spec
	updated.Persistent, updated.VolumeID, updated.MountPath = persistent, volumeID, mountPath
	if err := domain.ValidateSpec(updated); err != nil {
		return App{}, err
	}
	if persistent {
		volume, found := s.volumes[accountID][volumeID]
		if !found || volume.State != "Ready" {
			return App{}, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Select a ready volume from this account."}
		}
		for id, other := range s.apps[accountID] {
			if id != appID && other.Spec.VolumeID == volumeID {
				return App{}, &domain.Error{Code: "VOLUME_ALREADY_BOUND", Field: "volumeId", Message: "This volume is already bound to another app."}
			}
		}
	}
	if app.Spec.Persistent == persistent && app.Spec.VolumeID == volumeID && app.Spec.MountPath == mountPath {
		return app, nil
	}
	before := map[string]any{"persistent": app.Spec.Persistent, "volumeId": app.Spec.VolumeID, "mountPath": app.Spec.MountPath}
	app.Spec = updated
	s.apps[accountID][appID] = app
	s.addOperationLocked(accountID, appID, "UPDATE_APP_STORAGE", "Succeeded", map[string]any{
		"actor": subject, "before": before,
		"after": map[string]any{"persistent": persistent, "volumeId": volumeID, "mountPath": mountPath},
	})
	return app, nil
}
