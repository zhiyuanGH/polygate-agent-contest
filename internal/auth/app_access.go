package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"polygate/internal/server"
)

var appIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var privateDomainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

type AppTicketIssuer interface {
	IssueAppAccessTicket(context.Context, string, string, string) (string, error)
}

// AppAccessHandler exchanges an existing console login for an app-specific
// one-time ticket. The app gateway, not the student container, consumes it.
func (s *Service) AppAccessHandler(issuer AppTicketIssuer, privateDomain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if issuer == nil || len(privateDomain) > 200 || !privateDomainPattern.MatchString(privateDomain) ||
			!strings.Contains(privateDomain, ".") || strings.Contains(privateDomain, "..") {
			http.Error(w, "Private app access is unavailable", http.StatusServiceUnavailable)
			return
		}
		appID := r.URL.Query().Get("app")
		if !appIDPattern.MatchString(appID) {
			http.Error(w, "Invalid app ID", http.StatusBadRequest)
			return
		}
		returnPath := r.URL.Query().Get("return")
		if returnPath == "" {
			returnPath = "/"
		}
		if len(returnPath) > 2048 || !strings.HasPrefix(returnPath, "/") ||
			strings.HasPrefix(returnPath, "//") || strings.ContainsAny(returnPath, "\\\r\n\x00") {
			http.Error(w, "Invalid app return path", http.StatusBadRequest)
			return
		}
		principal, err := s.Current(r)
		if errors.Is(err, server.ErrUnavailable) {
			http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, server.ErrDisabled) {
			http.Error(w, "Account disabled", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		ticket, err := issuer.IssueAppAccessTicket(r.Context(), principal.AccountID, appID, returnPath)
		if errors.Is(err, server.ErrForbidden) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, "App authorization temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		destination := &url.URL{Scheme: "https", Host: appID + "." + privateDomain,
			Path: "/_polygate/auth/consume"}
		query := destination.Query()
		query.Set("ticket", ticket)
		destination.RawQuery = query.Encode()
		http.Redirect(w, r, destination.String(), http.StatusFound)
	})
}
