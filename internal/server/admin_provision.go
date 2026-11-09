package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"polygate/internal/keycloakadmin"
)

type AccountProvisionStore interface {
	ClaimAccountSlot(context.Context, string, string, string) (string, string, error)
	FinishAccountProvision(context.Context, string, string, string, string, string, string) (AdminAccount, error)
	StudentForPasswordReset(context.Context, string, string) (string, error)
	RecordPasswordReset(context.Context, string, string, string, string) error
}

type AccountManager interface {
	CreateTemporaryUser(context.Context, string, string, string, string) (string, bool, error)
	ResetTemporaryPassword(context.Context, string, string) error
	SetEnabled(context.Context, string, bool) error
	LogoutUser(context.Context, string) error
}

var studentUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

// NewAccountAdministrationHandler attaches account creation and password reset
// only when a realm-scoped Keycloak credential has been configured.
func NewAccountAdministrationHandler(base http.Handler, store Store,
	authenticate func(*http.Request) (Principal, error), manager AccountManager) http.Handler {
	h := &Handler{store: store, authenticate: authenticate}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts" {
			h.createAdminAccount(w, r, manager)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/admin/accounts/") &&
			strings.HasSuffix(r.URL.Path, "/reset-password") {
			h.resetAdminPassword(w, r, manager)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func readAccountJSON(w http.ResponseWriter, r *http.Request, input any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Send one valid JSON object.")
		return false
	}
	return true
}

func temporaryPassword() (string, error) {
	var raw [30]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (h *Handler) provisionAccess(w http.ResponseWriter, r *http.Request) (Principal, AccountProvisionStore, bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return Principal{}, nil, false
	}
	if p.Role != "admin" {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrator access is required.")
		return Principal{}, nil, false
	}
	store, ok := h.store.(AccountProvisionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISION_UNAVAILABLE", "Account provisioning is unavailable.")
		return Principal{}, nil, false
	}
	return p, store, true
}

func (h *Handler) createAdminAccount(w http.ResponseWriter, r *http.Request, manager AccountManager) {
	p, store, ok := h.provisionAccess(w, r)
	if !ok {
		return
	}
	var input struct {
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Reason      string `json:"reason"`
	}
	if !readAccountJSON(w, r, &input) {
		return
	}
	input.Username = strings.ToLower(strings.TrimSpace(input.Username))
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Reason = strings.TrimSpace(input.Reason)
	if !studentUsername.MatchString(input.Username) ||
		utf8.RuneCountInString(input.DisplayName) < 1 || utf8.RuneCountInString(input.DisplayName) > 100 ||
		utf8.RuneCountInString(input.Reason) < 3 || utf8.RuneCountInString(input.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Provide a username, display name, and reason.")
		return
	}
	password, err := temporaryPassword()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISION_UNAVAILABLE", "Account provisioning is unavailable.")
		return
	}
	id, claim, err := store.ClaimAccountSlot(r.Context(), p.AccountID, input.Username, input.DisplayName)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	identityID, created, err := manager.CreateTemporaryUser(r.Context(), id, input.Username, input.DisplayName, password)
	if err == nil && !created {
		err = manager.ResetTemporaryPassword(r.Context(), identityID, password)
	}
	if err == nil {
		err = manager.SetEnabled(r.Context(), identityID, true)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "IDENTITY_PROVISION_PENDING", "Identity setup did not finish. Retry this username after five minutes.")
		return
	}
	account, err := store.FinishAccountProvision(r.Context(), p.AccountID, p.Subject, id, claim, identityID, input.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": account, "temporaryPassword": password})
}

func (h *Handler) resetAdminPassword(w http.ResponseWriter, r *http.Request, manager AccountManager) {
	p, store, ok := h.provisionAccess(w, r)
	if !ok {
		return
	}
	prefix := "/api/v1/admin/accounts/"
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/reset-password")
	if !accountIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "INVALID_ACCOUNT_ID", "A valid account ID is required.")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if !readAccountJSON(w, r, &input) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if utf8.RuneCountInString(input.Reason) < 3 || utf8.RuneCountInString(input.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_REASON", "A reason of 3 to 500 characters is required.")
		return
	}
	subject, err := store.StudentForPasswordReset(r.Context(), p.AccountID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	password, err := temporaryPassword()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "PASSWORD_RESET_UNAVAILABLE", "Password reset is unavailable.")
		return
	}
	if err = manager.ResetTemporaryPassword(r.Context(), subject, password); err != nil {
		writeError(w, http.StatusServiceUnavailable, "PASSWORD_RESET_UNAVAILABLE", "Password reset is unavailable.")
		return
	}
	if err = manager.LogoutUser(r.Context(), subject); err != nil {
		writeError(w, http.StatusServiceUnavailable, "PASSWORD_RESET_UNAVAILABLE", "Password reset is unavailable.")
		return
	}
	if err = store.RecordPasswordReset(r.Context(), p.AccountID, p.Subject, id, input.Reason); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"temporaryPassword": password})
}

var _ AccountManager = (*keycloakadmin.Client)(nil)
