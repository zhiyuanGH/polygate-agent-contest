package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"polygate/internal/domain"
)

var accountIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (h *Handler) adminStore(w http.ResponseWriter, r *http.Request) (Principal, AdminAccountStore, bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return Principal{}, nil, false
	}
	if p.Role != "admin" {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrator access is required.")
		return Principal{}, nil, false
	}
	store, ok := h.store.(AdminAccountStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "Account administration is unavailable.")
		return Principal{}, nil, false
	}
	return p, store, true
}

func (h *Handler) listAdminAccounts(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAdminAccounts(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listAdminAccountAudit(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAdminAccountAudit(r.Context(), p.AccountID, 100)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) setAdminAccountEnabled(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminStore(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	if !accountIDPattern.MatchString(targetID) {
		writeError(w, http.StatusBadRequest, "INVALID_ACCOUNT_ID", "A valid account ID is required.")
		return
	}
	var input struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "A valid JSON body is required.")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Only one JSON body is allowed.")
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if input.Enabled == nil || utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Enabled and a reason of 3 to 500 characters are required.")
		return
	}
	if targetID == p.AccountID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrators cannot change their own account status.")
		return
	}
	account, err := store.SetAdminAccountEnabled(r.Context(), p.AccountID, p.Subject, targetID, *input.Enabled, reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (h *Handler) adminQuotaStore(w http.ResponseWriter, r *http.Request) (Principal, AdminQuotaStore, bool) {
	p, _, ok := h.adminStore(w, r)
	if !ok {
		return Principal{}, nil, false
	}
	if !h.adminQuotaEnabled {
		writeError(w, http.StatusServiceUnavailable, "QUOTA_CHANGES_DISABLED", "Account quota changes are unavailable.")
		return Principal{}, nil, false
	}
	store, ok := h.store.(AdminQuotaStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "Account administration is unavailable.")
		return Principal{}, nil, false
	}
	return p, store, true
}

func (h *Handler) listAdminQuotaChanges(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminQuotaStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAdminQuotaChanges(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) previewAdminQuotaChange(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.adminQuotaStore(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	if !accountIDPattern.MatchString(targetID) {
		writeError(w, http.StatusBadRequest, "INVALID_ACCOUNT_ID", "A valid account ID is required.")
		return
	}
	if targetID == p.AccountID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrators cannot change their own quota.")
		return
	}
	var input struct {
		Quota domain.Quota `json:"quota"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Send one proposed quota.")
		return
	}
	if err := domain.ValidateAccountQuota(input.Quota); err != nil {
		writeStoreError(w, err)
		return
	}
	accounts, err := h.store.(AdminAccountStore).ListAdminAccounts(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var target *AdminAccount
	for i := range accounts {
		if accounts[i].ID == targetID {
			target = &accounts[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "The account was not found.")
		return
	}
	if target.Role != "student" {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Only student quotas can be changed.")
		return
	}
	apps, usage, err := h.store.List(r.Context(), targetID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	preview := AdminQuotaPreview{TargetAccountID: targetID, CurrentQuota: target.Quota,
		ProposedQuota: input.Quota, Configured: usage, OverQuota: []string{},
		Apps: make([]AdminQuotaPreviewApp, 0, len(apps)), CheckedAt: time.Now().UTC()}
	for _, app := range apps {
		preview.Apps = append(preview.Apps, AdminQuotaPreviewApp{
			ID: app.ID, Name: app.Spec.Name, Status: app.Status, Resources: app.Spec.Resources})
	}
	for _, item := range []struct {
		name    string
		current int64
		allowed int64
	}{
		{"apps", usage.Apps, input.Quota.Apps},
		{"pods", usage.Pods, input.Quota.Pods},
		{"memoryMiB", usage.MemoryMiB, input.Quota.MemoryMiB},
		{"cpuRequestMilli", usage.CPURequestMilli, input.Quota.CPURequestMilli},
		{"cpuLimitMilli", usage.CPULimitMilli, input.Quota.CPULimitMilli},
	} {
		if item.current > item.allowed {
			preview.OverQuota = append(preview.OverQuota, item.name)
		}
	}
	writeJSON(w, http.StatusOK, preview)
}

func (h *Handler) queueAdminQuotaChange(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminQuotaStore(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	if !accountIDPattern.MatchString(targetID) {
		writeError(w, http.StatusBadRequest, "INVALID_ACCOUNT_ID", "A valid account ID is required.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	var input struct {
		Quota  domain.Quota `json:"quota"`
		Reason string       `json:"reason"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "A valid JSON body is required.")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Only one JSON body is allowed.")
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_REASON", "A reason of 3 to 500 characters is required.")
		return
	}
	if err := domain.ValidateAccountQuota(input.Quota); err != nil {
		writeStoreError(w, err)
		return
	}
	change, replay, err := store.QueueAdminQuotaChange(r.Context(), p.AccountID, p.Subject, targetID, key, reason, input.Quota)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, change)
		return
	}
	writeJSON(w, http.StatusAccepted, change)
}
