package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"polygate/internal/domain"
	"polygate/internal/secretcatalog"
)

func (h *Handler) secretAccount(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return Principal{}, false
	}
	if h.secretCatalog == nil {
		writeError(w, http.StatusServiceUnavailable, "SECRET_FEATURE_DISABLED", "Environment secrets are unavailable.")
		return Principal{}, false
	}
	if _, err := h.store.Account(r.Context(), p.AccountID); err != nil {
		writeStoreError(w, err)
		return Principal{}, false
	}
	return p, true
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	p, ok := h.secretAccount(w, r)
	if !ok {
		return
	}
	keys, err := h.secretCatalog.List(r.Context(), p.AccountID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
		return
	}
	items := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, map[string]string{"name": key})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.secretAccount(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := domain.ValidateSecretKeys([]string{name}); err != nil {
		writeStoreError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 9<<10)
	defer r.Body.Close()
	var input struct {
		Value string `json:"value"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Send one environment secret value.")
		return
	}
	store, ok := h.store.(SecretTransactionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
		return
	}
	if err := store.PutSecretWithAudit(r.Context(), p.AccountID, p.Subject, name,
		func(ctx context.Context, operationID string) error {
			return h.secretCatalog.PutMarked(ctx, p.AccountID, name, input.Value, operationID)
		}); err != nil {
		switch {
		case errors.Is(err, secretcatalog.ErrInvalidValue):
			writeError(w, http.StatusBadRequest, "INVALID_SECRET_VALUE", "Secret value must contain 1 to 8192 bytes.")
		case errors.Is(err, secretcatalog.ErrLimit):
			writeError(w, http.StatusConflict, "SECRET_LIMIT", "This account has reached its environment secret limit.")
		default:
			writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.secretAccount(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := domain.ValidateSecretKeys([]string{name}); err != nil {
		writeStoreError(w, err)
		return
	}
	store, ok := h.store.(SecretTransactionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
		return
	}
	err := store.DeleteSecretIfUnused(r.Context(), p.AccountID, p.Subject, name,
		func(ctx context.Context, operationID string) error {
			return h.secretCatalog.DeleteMarked(ctx, p.AccountID, name, operationID)
		})
	if err != nil {
		if errors.Is(err, secretcatalog.ErrMissing) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Secret key not found.")
		} else if errors.Is(err, ErrSecretInUse) {
			writeError(w, http.StatusConflict, "SECRET_IN_USE", "Delete apps that use this secret before removing it.")
		} else {
			writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
