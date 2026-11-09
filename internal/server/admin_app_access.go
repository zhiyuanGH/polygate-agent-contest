package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

func (h *Handler) adminAppStore(w http.ResponseWriter, r *http.Request) (Principal, AdminAppAccessStore, bool) {
	p, _, ok := h.adminStore(w, r)
	if !ok {
		return Principal{}, nil, false
	}
	store, ok := h.store.(AdminAppAccessStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "APP_ACCESS_UNAVAILABLE", "App access administration is unavailable.")
		return Principal{}, nil, false
	}
	return p, store, true
}

func (h *Handler) listAdminApps(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminAppStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAdminApps(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listAdminAppAccessAudit(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminAppStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAdminAppAccessAudit(r.Context(), p.AccountID, 100)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) setAdminAppAccess(w http.ResponseWriter, r *http.Request) {
	p, store, ok := h.adminAppStore(w, r)
	if !ok {
		return
	}
	appID := strings.ToLower(r.PathValue("id"))
	if !accountIDPattern.MatchString(appID) {
		writeError(w, http.StatusBadRequest, "INVALID_APP_ID", "A valid app ID is required.")
		return
	}
	var input struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
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
	if (input.Mode != "Public" && input.Mode != "Owner and administrators") ||
		utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_ACCESS_CHANGE", "Choose an access mode and enter a reason of 3 to 500 characters.")
		return
	}
	if input.Mode == "Owner and administrators" {
		if h.privateRouteCheck == nil {
			writeError(w, http.StatusServiceUnavailable, "PRIVATE_ROUTE_UNAVAILABLE", "Private app routing is not configured.")
			return
		}
		if err := h.privateRouteCheck(r.Context(), appID); err != nil {
			writeError(w, http.StatusConflict, "PRIVATE_ROUTE_NOT_READY", "Configure the app's private Cloudflare Tunnel route and retry.")
			return
		}
	}
	app, err := store.SetAdminAppAccess(r.Context(), p.AccountID, p.Subject, appID, input.Mode, reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func NewHTTPPrivateAppRouteChecker(domain string) (func(context.Context, string) error, error) {
	if domain != "polylink.org" {
		return nil, errors.New("private app domain must be polylink.org")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{Proxy: nil,
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSHandshakeTimeout: 3 * time.Second, ForceAttemptHTTP2: true},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return newPrivateRouteChecker(domain, client), nil
}

func newPrivateRouteChecker(domain string, client *http.Client) func(context.Context, string) error {
	return func(ctx context.Context, appID string) error {
		if !accountIDPattern.MatchString(appID) {
			return errors.New("invalid app ID")
		}
		endpoint := &url.URL{Scheme: "https", Host: strings.ToLower(appID) + "." + domain,
			Path: "/_polygate/route-check"}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent ||
			response.Header.Get("X-PolyGate-Route-App-ID") != strings.ToLower(appID) {
			return fmt.Errorf("private app route returned status %d or wrong gateway marker", response.StatusCode)
		}
		return nil
	}
}
