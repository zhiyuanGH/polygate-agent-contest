package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"polygate/internal/server"
)

const sessionTTL = 8 * time.Hour
const flowTTL = 5 * time.Minute

type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	BaseURL      string
	CSRFKey      []byte
}

type Service struct {
	pool       *pgxpool.Pool
	oauth      oauth2.Config
	verifier   *oidc.IDTokenVerifier
	baseOrigin string
	logoutURL  string
	csrfKey    []byte
	secure     bool
}

func New(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*Service, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && !(base.Scheme == "http" && strings.HasPrefix(base.Host, "127.0.0.1:"))) {
		return nil, errors.New("invalid base URL")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Scheme != base.Scheme || redirect.Host != base.Host || redirect.Path != "/auth/callback" {
		return nil, errors.New("OIDC redirect must use the base URL and /auth/callback")
	}
	if len(cfg.CSRFKey) < 32 {
		return nil, errors.New("CSRF key must contain at least 32 bytes")
	}
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("OIDC configuration is incomplete")
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	var discovery struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, err
	}
	logoutURL, err := url.Parse(discovery.EndSessionEndpoint)
	if err != nil || logoutURL.Host == "" ||
		(logoutURL.Scheme != "https" && !(logoutURL.Scheme == "http" && strings.HasPrefix(logoutURL.Host, "127.0.0.1:"))) {
		return nil, errors.New("OIDC logout endpoint is invalid")
	}
	return &Service{pool: pool, oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
		Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile"}},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}), baseOrigin: base.Scheme + "://" + base.Host,
		logoutURL: logoutURL.String(), csrfKey: cfg.CSRFKey, secure: base.Scheme == "https"}, nil
}

func (s *Service) sessionCookieName() string {
	if s.secure {
		return "__Host-polygate-session"
	}
	return "pg-dev-session"
}
func (s *Service) flowCookieName() string {
	if s.secure {
		return "__Host-polygate-flow"
	}
	return "pg-dev-flow"
}
func (s *Service) idTokenCookieName() string {
	if s.secure {
		return "__Host-polygate-id-token"
	}
	return "pg-dev-id-token"
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func tokenHash(value string) []byte { h := sha256.Sum256([]byte(value)); return h[:] }

func (s *Service) csrfToken(sessionID string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}

func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	nonce, err := randomToken()
	if err != nil {
		authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	verifier := oauth2.GenerateVerifier()
	flow, err := randomToken()
	if err != nil {
		authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	_, err = s.pool.Exec(r.Context(), `INSERT INTO auth_flows(flow_hash,state_hash,nonce_hash,pkce_verifier,expires_at)
        VALUES($1,$2,$3,$4,$5)`, tokenHash(flow), tokenHash(state), tokenHash(nonce), verifier, time.Now().Add(flowTTL))
	if err != nil {
		authError(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable.")
		return
	}
	s.setCookie(w, s.flowCookieName(), flow, int(flowTTL.Seconds()))
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(s.flowCookieName())
	if err != nil || c.Value == "" {
		authError(w, http.StatusBadRequest, "Sign-in session expired. Please try again.")
		return
	}
	s.setCookie(w, s.flowCookieName(), "", -1)
	var stateHash, nonceHash []byte
	var verifier string
	err = s.pool.QueryRow(r.Context(), `DELETE FROM auth_flows WHERE flow_hash=$1 AND expires_at>now()
        RETURNING state_hash,nonce_hash,pkce_verifier`, tokenHash(c.Value)).Scan(&stateHash, &nonceHash, &verifier)
	if err != nil {
		authError(w, http.StatusBadRequest, "Sign-in session expired. Please try again.")
		return
	}
	if subtle.ConstantTimeCompare(stateHash, tokenHash(r.URL.Query().Get("state"))) != 1 || r.URL.Query().Get("code") == "" {
		authError(w, http.StatusBadRequest, "Invalid sign-in response.")
		return
	}
	token, err := s.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		authError(w, http.StatusBadRequest, "Could not complete sign-in.")
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		authError(w, http.StatusBadRequest, "Invalid identity response.")
		return
	}
	id, err := s.verifier.Verify(r.Context(), rawID)
	if err != nil || id == nil || subtle.ConstantTimeCompare(nonceHash, tokenHash(id.Nonce)) != 1 {
		authError(w, http.StatusBadRequest, "Invalid identity response.")
		return
	}
	var accountID string
	var enabled bool
	err = s.pool.QueryRow(r.Context(), `SELECT id::text,enabled FROM accounts WHERE oidc_subject=$1`, id.Subject).Scan(&accountID, &enabled)
	if err != nil || !enabled {
		authError(w, http.StatusForbidden, "Your account is not available. Contact an administrator.")
		return
	}
	sessionID, err := randomToken()
	if err != nil {
		authError(w, http.StatusInternalServerError, "Could not create a session.")
		return
	}
	_, err = s.pool.Exec(r.Context(), `INSERT INTO auth_sessions(session_hash,account_id,csrf_hash,expires_at)
        VALUES($1,$2,$3,$4)`, tokenHash(sessionID), accountID, tokenHash(s.csrfToken(sessionID)), time.Now().Add(sessionTTL))
	if err != nil {
		authError(w, http.StatusServiceUnavailable, "Could not create a session.")
		return
	}
	s.setCookie(w, s.sessionCookieName(), sessionID, int(sessionTTL.Seconds()))
	s.setCookie(w, s.idTokenCookieName(), rawID, int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Service) current(r *http.Request) (server.Principal, []byte, error) {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil || c.Value == "" {
		return server.Principal{}, nil, errors.New("unauthenticated")
	}
	var p server.Principal
	var enabled bool
	var csrfHash []byte
	err = s.pool.QueryRow(r.Context(), `SELECT a.id::text,a.oidc_subject,a.display_name,a.role,a.enabled,s.csrf_hash
        FROM auth_sessions s JOIN accounts a ON a.id=s.account_id
        WHERE s.session_hash=$1 AND s.expires_at>now()`, tokenHash(c.Value)).Scan(
		&p.AccountID, &p.Subject, &p.DisplayName, &p.Role, &enabled, &csrfHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.Principal{}, nil, errors.New("unauthenticated")
	}
	if err != nil {
		return server.Principal{}, nil, server.ErrUnavailable
	}
	if !enabled {
		return server.Principal{}, nil, server.ErrDisabled
	}
	return p, csrfHash, nil
}

func (s *Service) Current(r *http.Request) (server.Principal, error) {
	p, _, err := s.current(r)
	return p, err
}

func (s *Service) checkCSRF(r *http.Request) bool {
	if r.Header.Get("Origin") != s.baseOrigin {
		return false
	}
	_, expected, err := s.current(r)
	if err != nil {
		return false
	}
	actual := tokenHash(r.Header.Get("X-CSRF-Token"))
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

func (s *Service) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && !s.checkCSRF(r) {
			authError(w, http.StatusForbidden, "Request verification failed. Refresh the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) CSRF(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.current(r); err != nil {
		if errors.Is(err, server.ErrUnavailable) {
			authError(w, http.StatusServiceUnavailable, "The service is temporarily unavailable.")
			return
		}
		authError(w, http.StatusUnauthorized, "Sign in to continue.")
		return
	}
	c, _ := r.Cookie(s.sessionCookieName())
	writeJSON(w, http.StatusOK, map[string]string{"token": s.csrfToken(c.Value)})
}

func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		authError(w, http.StatusForbidden, "Request verification failed.")
		return
	}
	c, _ := r.Cookie(s.sessionCookieName())
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM auth_sessions WHERE session_hash=$1`, tokenHash(c.Value)); err != nil {
		authError(w, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable.")
		return
	}
	s.setCookie(w, s.sessionCookieName(), "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"signedOut": true})
}

func (s *Service) OIDCLogout(w http.ResponseWriter, r *http.Request) {
	endpoint, err := url.Parse(s.logoutURL)
	if err != nil || endpoint.Host == "" {
		authError(w, http.StatusServiceUnavailable, "Identity sign-out is unavailable.")
		return
	}
	query := endpoint.Query()
	query.Set("client_id", s.oauth.ClientID)
	query.Set("post_logout_redirect_uri", s.baseOrigin+"/")
	if cookie, err := r.Cookie(s.idTokenCookieName()); err == nil && cookie.Value != "" {
		query.Set("id_token_hint", cookie.Value)
	}
	endpoint.RawQuery = query.Encode()
	s.setCookie(w, s.idTokenCookieName(), "", -1)
	http.Redirect(w, r, endpoint.String(), http.StatusFound)
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.Login)
	mux.HandleFunc("GET /auth/callback", s.Callback)
	mux.HandleFunc("GET /auth/csrf", s.CSRF)
	mux.HandleFunc("POST /auth/logout", s.Logout)
	mux.HandleFunc("GET /auth/oidc-logout", s.OIDCLogout)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func authError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": "AUTH_ERROR", "message": message}})
}
