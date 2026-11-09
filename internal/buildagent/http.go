package buildagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"polygate/internal/source"
)

const WorkerURI = "spiffe://polygate/build-worker"

type Handler struct {
	manager     *Manager
	publicKey   ed25519.PublicKey
	stagingRoot string
	uploads     chan struct{}
	replay      *ReplayLog
	now         func() time.Time
}

func NewHandler(manager *Manager, publicKey ed25519.PublicKey, stagingRoot string) (http.Handler, error) {
	if manager == nil || len(publicKey) != ed25519.PublicKeySize || !filepath.IsAbs(stagingRoot) {
		return nil, errors.New("invalid build agent handler configuration")
	}
	if err := secureOwnerDirectory(stagingRoot); err != nil {
		return nil, err
	}
	replay, err := NewReplayLog(stagingRoot)
	if err != nil {
		return nil, err
	}
	h := &Handler{manager: manager, publicKey: publicKey, stagingRoot: stagingRoot,
		uploads: make(chan struct{}, SlotCount), replay: replay, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /v1/builds", h.build)
	return mux, nil
}

func (h *Handler) authorized(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		if uri.String() == WorkerURI {
			return true
		}
	}
	return false
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ready, busy, quarantined := h.manager.Status()
	status := http.StatusOK
	if !ready || busy+quarantined >= SlotCount {
		status = http.StatusServiceUnavailable
	}
	writeAgentJSON(w, status, map[string]any{"ready": ready, "busy": busy, "quarantined": quarantined})
}

func (h *Handler) build(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		agentError(w, http.StatusForbidden, "UNAUTHORIZED_WORKER")
		return
	}
	grantHeader := r.Header.Get("X-Polygate-Grant")
	signature := r.Header.Get("X-Polygate-Signature")
	if len(grantHeader) == 0 || len(grantHeader) > 2048 || len(signature) == 0 || len(signature) > 128 {
		agentError(w, http.StatusForbidden, "INVALID_GRANT")
		return
	}
	encoded, err := base64.RawURLEncoding.DecodeString(grantHeader)
	if err != nil || len(encoded) > 1024 {
		agentError(w, http.StatusForbidden, "INVALID_GRANT")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var grant Grant
	if decoder.Decode(&grant) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		VerifyGrant(h.publicKey, grant, signature, h.now()) != nil {
		agentError(w, http.StatusForbidden, "INVALID_GRANT")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/zip" {
		agentError(w, http.StatusUnsupportedMediaType, "ZIP_REQUIRED")
		return
	}
	if r.ContentLength > source.MaxArchiveBytes {
		agentError(w, http.StatusRequestEntityTooLarge, "SOURCE_TOO_LARGE")
		return
	}
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	default:
		w.Header().Set("Retry-After", "5")
		agentError(w, http.StatusTooManyRequests, "BUILD_HOST_BUSY")
		return
	}
	ready, _, _ := h.manager.Status()
	if !ready {
		agentError(w, http.StatusServiceUnavailable, "BUILD_HOST_NOT_READY")
		return
	}
	tmp, err := os.CreateTemp(h.stagingRoot, ".upload-*.zip")
	if err != nil {
		agentError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = tmp.Chmod(0600); err != nil {
		agentError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, source.MaxArchiveBytes+1)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), r.Body)
	if err != nil || size > source.MaxArchiveBytes {
		agentError(w, http.StatusRequestEntityTooLarge, "SOURCE_TOO_LARGE")
		return
	}
	if size != grant.SourceBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != grant.SourceSHA256 {
		agentError(w, http.StatusBadRequest, "SOURCE_MISMATCH")
		return
	}
	if err = tmp.Sync(); err != nil {
		agentError(w, http.StatusServiceUnavailable, "BUILD_STORAGE_UNAVAILABLE")
		return
	}
	if err = source.ValidateZIP(tmp, size); err != nil {
		agentError(w, http.StatusBadRequest, "INVALID_SOURCE_ARCHIVE")
		return
	}
	if err = h.replay.Claim(grant); err != nil {
		if errors.Is(err, ErrGrantReplayed) {
			agentError(w, http.StatusConflict, "GRANT_REPLAYED")
		} else {
			agentError(w, http.StatusServiceUnavailable, "GRANT_LEDGER_UNAVAILABLE")
		}
		return
	}
	streaming := r.Header.Get("Accept") == "application/x-ndjson"
	var stream *buildEventStream
	var logWriter io.Writer
	if streaming {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		stream = &buildEventStream{writer: w, encoder: json.NewEncoder(w)}
		logWriter = stream
	}
	result, err := h.manager.Run(r.Context(), Request{TaskID: grant.TaskID, Attempt: grant.Attempt,
		AccountID: grant.AccountID, SourceSHA256: grant.SourceSHA256, SourceBytes: size, Source: tmp, LogWriter: logWriter})
	if err != nil {
		code, status := agentFailure(err)
		if streaming {
			_ = stream.send(buildEvent{Type: "error", Code: code})
			return
		}
		agentError(w, status, code)
		return
	}
	if !sha256Pattern.MatchString(result.ImageDigest) || !sha256Pattern.MatchString(result.LogSHA256) {
		if streaming {
			_ = stream.send(buildEvent{Type: "error", Code: "INVALID_BUILD_RESULT"})
			return
		}
		agentError(w, http.StatusInternalServerError, "INVALID_BUILD_RESULT")
		return
	}
	if streaming {
		_ = stream.send(buildEvent{Type: "result", Result: &result})
		return
	}
	writeAgentJSON(w, http.StatusOK, result)
}

func agentFailure(err error) (string, int) {
	switch {
	case errors.Is(err, ErrNotReady):
		return "BUILD_HOST_NOT_READY", http.StatusServiceUnavailable
	case errors.Is(err, ErrBusy):
		return "BUILD_HOST_BUSY", http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		return "BUILD_TIMEOUT", http.StatusGatewayTimeout
	default:
		return "BUILD_FAILED", http.StatusInternalServerError
	}
}

type buildEvent struct {
	Type   string  `json:"type"`
	Data   string  `json:"data,omitempty"`
	Code   string  `json:"code,omitempty"`
	Result *Result `json:"result,omitempty"`
}

type buildEventStream struct {
	writer  http.ResponseWriter
	encoder *json.Encoder
}

func (s *buildEventStream) send(event buildEvent) error {
	if err := s.encoder.Encode(event); err != nil {
		return err
	}
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (s *buildEventStream) Write(data []byte) (int, error) {
	if err := s.send(buildEvent{Type: "log", Data: base64.StdEncoding.EncodeToString(data)}); err != nil {
		return 0, err
	}
	return len(data), nil
}

func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func agentError(w http.ResponseWriter, status int, code string) {
	writeAgentJSON(w, status, map[string]any{"error": map[string]string{"code": strings.ToUpper(code)}})
}
