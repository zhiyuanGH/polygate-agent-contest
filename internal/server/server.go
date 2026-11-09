package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"polygate/internal/applogs"
	"polygate/internal/domain"
	"polygate/internal/registry"
	"polygate/internal/source"
)

type App struct {
	ID        string         `json:"id"`
	Spec      domain.AppSpec `json:"spec"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
}

type Handler struct {
	mux                *http.ServeMux
	store              Store
	authenticate       func(*http.Request) (Principal, error)
	mode               string
	trustedImageDigest string
	trustedImagePort   int
	imageDeployEnabled bool
	imageInspector     func(context.Context, string) (registry.Image, error)
	imageInspectSlots  chan struct{}
	buildRoot          string
	buildEnabled       bool
	volumeEnabled      bool
	adminQuotaEnabled  bool
	appLogReader       AppLogReader
	secretCatalog      SecretCatalog
	imageUploader      func(context.Context, string, string, string) (registry.Image, error)
	imageUploadSlots   chan struct{}
	imageReportReader  func(context.Context, string, string, string) ([]byte, error)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// SetImageUploader enables account-scoped docker-save imports before serving.
func (h *Handler) SetImageUploader(upload func(context.Context, string, string, string) (registry.Image, error)) {
	h.imageUploader = upload
	h.imageUploadSlots = make(chan struct{}, 1)
}

func (h *Handler) SetImageReportReader(read func(context.Context, string, string, string) ([]byte, error)) {
	h.imageReportReader = read
}

type AppLogReader interface {
	Read(context.Context, string, string, string, bool) (applogs.Result, error)
	List(context.Context, string, string) ([]applogs.PodInfo, error)
	Events(context.Context, string, string) ([]applogs.Event, error)
	Metrics(context.Context, string, string) ([]applogs.PodMetric, error)
}

type SecretCatalog interface {
	List(context.Context, string) ([]string, error)
	HasKeys(context.Context, string, []string) (bool, error)
	PutMarked(context.Context, string, string, string, string) error
	DeleteMarked(context.Context, string, string, string) error
}

type SecretTransactionStore interface {
	CreateWithSecretCheck(context.Context, string, string, string, domain.AppSpec,
		func(context.Context) error) (App, bool, error)
	PutSecretWithAudit(context.Context, string, string, string, func(context.Context, string) error) error
	DeleteSecretIfUnused(context.Context, string, string, string, func(context.Context, string) error) error
}

var ErrSecretKeysMissing = errors.New("selected environment secret is missing")
var ErrSecretInUse = errors.New("environment secret is referenced by an app")

func NewHandler(store Store, authenticate func(*http.Request) (Principal, error)) http.Handler {
	return newHandler(store, authenticate, "authenticated", "", 0, nil, false, "", false, nil)
}

func NewHandlerWithTrustedImage(store Store, authenticate func(*http.Request) (Principal, error), imageDigest string, imagePort int) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort, nil, false, "", false, nil)
}

func NewHandlerWithImageInspection(store Store, authenticate func(*http.Request) (Principal, error),
	imageDigest string, imagePort int, inspector func(context.Context, string) (registry.Image, error)) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort, inspector, false, "", false, nil)
}

func NewHandlerWithImageDeployment(store Store, authenticate func(*http.Request) (Principal, error),
	imageDigest string, imagePort int, inspector func(context.Context, string) (registry.Image, error),
	imageDeployEnabled bool) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort,
		inspector, imageDeployEnabled, "", false, nil)
}

func NewHandlerWithBuildSubmission(store Store, authenticate func(*http.Request) (Principal, error),
	imageDigest string, imagePort int, inspector func(context.Context, string) (registry.Image, error),
	imageDeployEnabled bool, buildRoot string) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort,
		inspector, imageDeployEnabled, buildRoot, true, nil)
}

func NewHandlerWithAppLogs(store Store, authenticate func(*http.Request) (Principal, error),
	imageDigest string, imagePort int, inspector func(context.Context, string) (registry.Image, error),
	imageDeployEnabled bool, buildRoot string, buildEnabled bool, reader AppLogReader) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort,
		inspector, imageDeployEnabled, buildRoot, buildEnabled, nil, reader)
}

func NewHandlerWithAppLogsAndSecrets(store Store, authenticate func(*http.Request) (Principal, error),
	imageDigest string, imagePort int, inspector func(context.Context, string) (registry.Image, error),
	imageDeployEnabled bool, buildRoot string, buildEnabled bool, reader AppLogReader,
	secrets SecretCatalog) http.Handler {
	return newHandler(store, authenticate, "authenticated", imageDigest, imagePort,
		inspector, imageDeployEnabled, buildRoot, buildEnabled, secrets, reader)
}

func newHandler(store Store, authenticate func(*http.Request) (Principal, error), mode, imageDigest string,
	imagePort int, inspector func(context.Context, string) (registry.Image, error),
	imageDeployEnabled bool, buildRoot string, buildEnabled bool, secrets SecretCatalog, readers ...AppLogReader) http.Handler {
	h := &Handler{store: store, authenticate: authenticate, mode: mode, trustedImageDigest: imageDigest,
		trustedImagePort: imagePort, imageInspector: inspector, imageInspectSlots: make(chan struct{}, 2),
		imageDeployEnabled: imageDeployEnabled, buildRoot: buildRoot, buildEnabled: buildEnabled,
		volumeEnabled: mode == "loopback-development" || os.Getenv("POLYGATE_VOLUME_ENABLED") == "1",
		secretCatalog: secrets}
	h.adminQuotaEnabled = os.Getenv("POLYGATE_ADMIN_QUOTA_ENABLED") == "1"
	if len(readers) != 0 {
		h.appLogReader = readers[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": mode})
	})
	mux.HandleFunc("GET /api/v1/me", h.me)
	mux.HandleFunc("GET /api/v1/usage/actual", h.accountActualUsage)
	mux.HandleFunc("GET /api/v1/admin/accounts", h.listAdminAccounts)
	mux.HandleFunc("GET /api/v1/admin/cluster-resources", h.adminClusterResources)
	mux.HandleFunc("PATCH /api/v1/admin/accounts/{id}/enabled", h.setAdminAccountEnabled)
	mux.HandleFunc("GET /api/v1/admin/account-audit", h.listAdminAccountAudit)
	mux.HandleFunc("GET /api/v1/admin/quota-changes", h.listAdminQuotaChanges)
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/quota-preview", h.previewAdminQuotaChange)
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/quota", h.queueAdminQuotaChange)
	mux.HandleFunc("GET /api/v1/apps", h.listApps)
	mux.HandleFunc("GET /api/v1/operations", h.listOperations)
	mux.HandleFunc("GET /api/v1/operations/history", h.listOperationHistory)
	mux.HandleFunc("GET /api/v1/images", h.listImages)
	mux.HandleFunc("GET /api/v1/images/{id}/scan-report", h.imageScanReport)
	mux.HandleFunc("GET /api/v1/images/{id}/scan-failure", h.imageScanFailure)
	mux.HandleFunc("GET /api/v1/secrets", h.listSecrets)
	mux.HandleFunc("PUT /api/v1/secrets/{name}", h.putSecret)
	mux.HandleFunc("DELETE /api/v1/secrets/{name}", h.deleteSecret)
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.deleteImage)
	mux.HandleFunc("GET /api/v1/volumes", h.listVolumes)
	mux.HandleFunc("GET /api/v1/volumes/{id}", h.getVolume)
	mux.HandleFunc("POST /api/v1/volumes", h.createVolume)
	mux.HandleFunc("PATCH /api/v1/volumes/{id}", h.expandVolume)
	mux.HandleFunc("DELETE /api/v1/volumes/{id}", h.deleteVolume)
	mux.HandleFunc("POST /api/v1/images/inspect", h.inspectImage)
	mux.HandleFunc("POST /api/v1/images/uploads", h.createImageUpload)
	mux.HandleFunc("PUT /api/v1/images/uploads/{id}/chunks/{index}", h.putImageUploadChunk)
	mux.HandleFunc("POST /api/v1/images/uploads/{id}/complete", h.completeImageUpload)
	mux.HandleFunc("GET /api/v1/apps/{id}", h.getApp)
	mux.HandleFunc("PATCH /api/v1/apps/{id}/resources", h.updateAppResources)
	mux.HandleFunc("PATCH /api/v1/apps/{id}/runtime", h.updateAppRuntime)
	mux.HandleFunc("POST /api/v1/apps/{id}/update-resources", h.updateRunningResources)
	mux.HandleFunc("POST /api/v1/apps/{id}/restart", h.restartRunningApp)
	mux.HandleFunc("GET /api/v1/apps/{id}/revisions", h.listRevisions)
	mux.HandleFunc("GET /api/v1/apps/{id}/status", h.getDeploymentStatus)
	mux.HandleFunc("GET /api/v1/apps/{id}/logs", h.getAppLogs)
	mux.HandleFunc("GET /api/v1/apps/{id}/pods", h.getAppPods)
	mux.HandleFunc("GET /api/v1/apps/{id}/logs/history", h.getAppLogHistory)
	mux.HandleFunc("GET /api/v1/apps/{id}/events", h.getAppEvents)
	mux.HandleFunc("GET /api/v1/apps/{id}/metrics", h.getAppMetrics)
	mux.HandleFunc("POST /api/v1/apps/{id}/deploy", h.deployApp)
	mux.HandleFunc("POST /api/v1/apps/{id}/deploy-image", h.deployApprovedImage)
	mux.HandleFunc("POST /api/v1/apps/{id}/update-image", h.updateApprovedImage)
	mux.HandleFunc("POST /api/v1/apps/{id}/rollback", h.rollbackRevision)
	mux.HandleFunc("POST /api/v1/apps/{id}/stop", h.stopApp)
	mux.HandleFunc("POST /api/v1/apps", h.createApp)
	mux.HandleFunc("DELETE /api/v1/apps/{id}", h.deleteApp)
	mux.HandleFunc("POST /api/v1/source/validate", h.validateSource)
	mux.HandleFunc("POST /api/v1/source/builds", h.submitSourceBuild)
	mux.HandleFunc("POST /api/v1/source/uploads", h.createSourceUpload)
	mux.HandleFunc("PUT /api/v1/source/uploads/{id}/chunks/{index}", h.putSourceUploadChunk)
	mux.HandleFunc("POST /api/v1/source/uploads/{id}/complete", h.completeSourceUpload)
	mux.HandleFunc("GET /api/v1/source/builds", h.listSourceBuilds)
	mux.HandleFunc("GET /api/v1/source/builds/{id}/log", h.sourceBuildLog)
	h.mux = mux
	return h
}

func (h *Handler) volumeStore(ctx context.Context, w http.ResponseWriter, accountID string) (VolumeStore, bool) {
	store, ok := h.store.(VolumeStore)
	if !ok || !h.volumeEnabled {
		writeError(w, http.StatusServiceUnavailable, "VOLUME_CATALOG_DISABLED", "Volume reservations are unavailable.")
		return nil, false
	}
	account, err := h.store.Account(ctx, accountID)
	if err != nil {
		writeStoreError(w, err)
		return nil, false
	}
	if !account.VolumeEnabled {
		writeError(w, http.StatusForbidden, "VOLUME_PILOT_DISABLED", "Volumes are not enabled for this account.")
		return nil, false
	}
	return store, true
}

func (h *Handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.volumeStore(r.Context(), w, p.AccountID)
	if !ok {
		return
	}
	items, err := store.ListVolumes(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getVolume(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.volumeStore(r.Context(), w, p.AccountID)
	if !ok {
		return
	}
	item, err := store.GetVolume(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) createVolume(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.volumeStore(r.Context(), w, p.AccountID)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	defer r.Body.Close()
	var spec domain.VolumeSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Send one valid volume configuration.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	item, replay, err := store.CreateVolume(r.Context(), p.AccountID, p.Subject, key, spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, item)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (h *Handler) expandVolume(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.volumeStore(r.Context(), w, p.AccountID)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	defer r.Body.Close()
	var input struct {
		SizeGiB int `json:"sizeGiB"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Send one valid volume size.")
		return
	}
	item, err := store.ExpandVolume(r.Context(), p.AccountID, p.Subject, r.PathValue("id"), input.SizeGiB)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) deleteVolume(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.volumeStore(r.Context(), w, p.AccountID)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	defer r.Body.Close()
	var input struct {
		ConfirmName string `json:"confirmName"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Send one deletion confirmation.")
		return
	}
	item, err := store.DeleteVolume(r.Context(), p.AccountID, p.Subject, r.PathValue("id"), input.ConfirmName)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, item)
}

func NewDevHandler(store Store) http.Handler {
	base := newHandler(store, func(_ *http.Request) (Principal, error) {
		return Principal{AccountID: "local-student", Subject: "local-student", DisplayName: "Local Student", Role: "student"}, nil
	}, "loopback-development", "", 0, nil, false, "", false, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": "loopback-development"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/auth/csrf" {
			writeJSON(w, http.StatusOK, map[string]string{"token": "local-development"})
			return
		}
		base.ServeHTTP(w, r)
	})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	account, err := h.store.Account(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_, usage, err := h.store.List(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	message := "Apps are saved as drafts and are not deployed."
	_, deployStoreAvailable := h.store.(TrustedDeployStore)
	deployAvailable := deployStoreAvailable && h.trustedImageDigest != "" && h.trustedImagePort > 0
	if deployAvailable {
		message = "Trusted sample deployment is enabled. Student images are not deployed."
	}
	imageAvailable := h.imageDeployEnabled && account.ImageDeployEnabled
	buildAvailable := h.buildEnabled && h.buildRoot != "" && account.SourceBuildEnabled
	_, volumeStoreAvailable := h.store.(VolumeStore)
	if imageAvailable {
		message = "Approved image deployment is available for this account."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": account.ID, "displayName": account.DisplayName, "role": account.Role,
		"quota": account.Quota, "configured": usage, "mode": h.mode,
		"trustedDeploymentAvailable": deployAvailable,
		"imageDeploymentAvailable":   imageAvailable,
		"sourceBuildAvailable":       buildAvailable,
		"volumeAvailable":            h.volumeEnabled && volumeStoreAvailable && account.VolumeEnabled,
		"appLogsAvailable":           h.appLogReader != nil,
		"appLogHistoryAvailable":     h.appLogReader != nil && os.Getenv("POLYGATE_LOKI_URL") != "",
		"secretAvailable":            h.secretCatalog != nil,
		"appAccessBaseURL":           os.Getenv("POLYGATE_APP_ACCESS_BASE_URL"),
		"adminQuotaAvailable":        account.Role == "admin" && h.adminQuotaEnabled,
		"adminProvisionAvailable":    account.Role == "admin" && os.Getenv("POLYGATE_ADMIN_PROVISION_ENABLED") == "1",
		"message":                    message,
	})
}

func (h *Handler) listApps(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	apps, _, err := h.store.List(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": apps})
}

func (h *Handler) listOperations(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	ops, err := h.store.ListOperations(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ops})
}

func (h *Handler) getApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	app, err := h.store.Get(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) updateAppResources(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	editor, available := h.store.(AppResourceEditor)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "RESOURCE_EDIT_UNAVAILABLE", "Resource editing is unavailable.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512)
	defer r.Body.Close()
	var input struct {
		Resources domain.Resources `json:"resources"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_RESOURCE_EDIT", "Send one resource settings object.")
		return
	}
	app, err := editor.UpdateAppResources(r.Context(), p.AccountID, p.Subject, r.PathValue("id"), input.Resources)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) updateAppRuntime(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	editor, available := h.store.(AppRuntimeEditor)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "RUNTIME_EDIT_UNAVAILABLE", "App runtime editing is unavailable.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512)
	defer r.Body.Close()
	var input struct {
		Port       int    `json:"port"`
		HealthPath string `json:"healthPath"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_RUNTIME_EDIT", "Send one port and health path object.")
		return
	}
	app, err := editor.UpdateAppRuntime(r.Context(), p.AccountID, p.Subject,
		r.PathValue("id"), input.Port, input.HealthPath)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) updateRunningResources(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	updater, available := h.store.(RunningResourceUpdateStore)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "RESOURCE_UPDATE_UNAVAILABLE", "Running resource updates are unavailable.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512)
	defer r.Body.Close()
	var input struct {
		Resources domain.Resources `json:"resources"`
		Strategy  string           `json:"strategy"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_RESOURCE_UPDATE", "Send one resource update request.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	task, replay, err := updater.QueueRunningResourceUpdate(r.Context(), p.AccountID,
		p.Subject, r.PathValue("id"), key, input.Resources, input.Strategy)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *Handler) restartRunningApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	restarter, available := h.store.(RunningRestartStore)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "RESTART_UNAVAILABLE", "App restart is unavailable.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	defer r.Body.Close()
	var input struct {
		Strategy string `json:"strategy"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		(input.Strategy != "Rolling" && input.Strategy != "Recreate") {
		writeError(w, http.StatusBadRequest, "INVALID_RESTART", "Choose Rolling or Recreate.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	task, replay, err := restarter.QueueRunningRestart(r.Context(), p.AccountID,
		p.Subject, r.PathValue("id"), key, input.Strategy)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *Handler) listRevisions(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(RevisionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "REVISION_HISTORY_UNAVAILABLE", "Revision history is unavailable.")
		return
	}
	before := 0
	if value := r.URL.Query().Get("before"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "INVALID_REVISION_CURSOR", "Use a positive revision number for before.")
			return
		}
		before = int(parsed)
	}
	page, err := store.ListRevisions(r.Context(), p.AccountID, r.PathValue("id"), before, 50)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) getDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	status, err := h.store.DeploymentStatus(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) getAppLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "APP_LOGS_DISABLED", "Application logs are unavailable.")
		return
	}
	appID := r.PathValue("id")
	if _, err := h.store.Get(r.Context(), p.AccountID, appID); err != nil {
		writeStoreError(w, err)
		return
	}
	requestedPod := r.URL.Query().Get("pod")
	if len(requestedPod) > 253 {
		writeError(w, http.StatusBadRequest, "INVALID_POD", "Select a valid app pod.")
		return
	}
	previousValue := r.URL.Query().Get("previous")
	if previousValue != "" && previousValue != "1" {
		writeError(w, http.StatusBadRequest, "INVALID_LOG_ATTEMPT", "Select the current or previous container attempt.")
		return
	}
	result, err := h.appLogReader.Read(r.Context(), p.AccountID, appID, requestedPod, previousValue == "1")
	if errors.Is(err, applogs.ErrNoPods) || errors.Is(err, applogs.ErrPodNotFound) {
		writeError(w, http.StatusNotFound, "APP_POD_NOT_FOUND", "No matching app pod is available.")
		return
	}
	if errors.Is(err, applogs.ErrNoPreviousLogs) {
		writeError(w, http.StatusNotFound, "PREVIOUS_LOGS_NOT_FOUND", "No previous container output is available for this Pod.")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "APP_LOGS_UNAVAILABLE", "Application logs could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) getAppPods(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "APP_LOGS_DISABLED", "Application pods are unavailable.")
		return
	}
	appID := r.PathValue("id")
	if _, err := h.store.Get(r.Context(), p.AccountID, appID); err != nil {
		writeStoreError(w, err)
		return
	}
	items, err := h.appLogReader.List(r.Context(), p.AccountID, appID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "APP_PODS_UNAVAILABLE", "Application pods could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getAppLogHistory(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "APP_HISTORY_DISABLED", "Historical logs are unavailable.")
		return
	}
	appID := r.PathValue("id")
	if _, err := h.store.Get(r.Context(), p.AccountID, appID); err != nil {
		writeStoreError(w, err)
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || (value != 24 && value != 168) {
			writeError(w, http.StatusBadRequest, "INVALID_LOG_RANGE", "Choose 24 hours or 7 days.")
			return
		}
		hours = value
	}
	before := r.URL.Query().Get("before")
	pod := r.URL.Query().Get("pod")
	reader, available := h.appLogReader.(interface {
		History(context.Context, string, string, int, string, string) (applogs.HistoryResult, error)
	})
	if !available {
		writeError(w, http.StatusServiceUnavailable, "APP_HISTORY_DISABLED", "Historical logs are unavailable.")
		return
	}
	result, err := reader.History(r.Context(), p.AccountID, appID, hours, before, pod)
	if errors.Is(err, applogs.ErrInvalidHistoryCursor) {
		writeError(w, http.StatusBadRequest, "INVALID_LOG_CURSOR", "Refresh the logs and choose an older page again.")
		return
	}
	if errors.Is(err, applogs.ErrInvalidHistoryPod) {
		writeError(w, http.StatusBadRequest, "INVALID_LOG_POD", "Choose a Pod that belongs to this app.")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "APP_HISTORY_UNAVAILABLE", "Historical logs could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) getAppEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "APP_EVENTS_DISABLED", "Application events are unavailable.")
		return
	}
	appID := r.PathValue("id")
	if _, err := h.store.Get(r.Context(), p.AccountID, appID); err != nil {
		writeStoreError(w, err)
		return
	}
	items, err := h.appLogReader.Events(r.Context(), p.AccountID, appID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "APP_EVENTS_UNAVAILABLE", "Application events could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getAppMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "APP_METRICS_DISABLED", "Application metrics are unavailable.")
		return
	}
	appID := r.PathValue("id")
	if _, err := h.store.Get(r.Context(), p.AccountID, appID); err != nil {
		writeStoreError(w, err)
		return
	}
	items, err := h.appLogReader.Metrics(r.Context(), p.AccountID, appID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "APP_METRICS_UNAVAILABLE", "Application metrics could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) deployApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	deployer, available := h.store.(TrustedDeployStore)
	if !available || h.trustedImageDigest == "" || h.trustedImagePort <= 0 {
		writeError(w, http.StatusServiceUnavailable, "DEPLOYMENT_DISABLED", "Trusted sample deployment is not available yet.")
		return
	}
	key, valid := emptyOperationRequest(w, r)
	if !valid {
		return
	}
	task, replay, err := deployer.QueueTrustedDeploy(r.Context(), p.AccountID, p.Subject, r.PathValue("id"), key, h.trustedImageDigest, h.trustedImagePort)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (h *Handler) stopApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	deployer, available := h.store.(StopStore)
	if !available {
		writeError(w, http.StatusServiceUnavailable, "DEPLOYMENT_DISABLED", "Deployment operations are not available yet.")
		return
	}
	key, valid := emptyOperationRequest(w, r)
	if !valid {
		return
	}
	task, replay, err := deployer.QueueStop(r.Context(), p.AccountID, p.Subject, r.PathValue("id"), key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func emptyOperationRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1)
	defer r.Body.Close()
	if body, err := io.ReadAll(r.Body); err != nil || len(body) != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Deployment operations must have an empty body.")
		return "", false
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return "", false
	}
	return key, true
}

func (h *Handler) deleteApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := h.store.Delete(r.Context(), p.AccountID, p.Subject, r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h *Handler) createApp(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	var spec domain.AppSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid app configuration.")
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Only one JSON object is allowed.")
		return
	}
	if err := domain.ValidateSpec(spec); err != nil {
		writeStoreError(w, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, " \r\n\t") {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Send an Idempotency-Key of 8–128 non-space characters.")
		return
	}
	if len(spec.SecretKeys) != 0 {
		store, ok := h.store.(SecretTransactionStore)
		if h.secretCatalog == nil || !ok {
			writeError(w, http.StatusServiceUnavailable, "SECRET_FEATURE_DISABLED", "Environment secrets are unavailable.")
			return
		}
		app, replay, err := store.CreateWithSecretCheck(r.Context(), p.AccountID, p.Subject, key, spec,
			func(ctx context.Context) error {
				present, checkErr := h.secretCatalog.HasKeys(ctx, p.AccountID, spec.SecretKeys)
				if checkErr != nil {
					return ErrUnavailable
				}
				if !present {
					return ErrSecretKeysMissing
				}
				return nil
			})
		if errors.Is(err, ErrSecretKeysMissing) {
			writeError(w, http.StatusBadRequest, "SECRET_KEY_MISSING", "Create the selected environment secrets before this app.")
			return
		}
		if errors.Is(err, ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "SECRET_UNAVAILABLE", "Environment secrets are unavailable.")
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if replay {
			writeJSON(w, http.StatusOK, app)
		} else {
			writeJSON(w, http.StatusCreated, app)
		}
		return
	}
	app, replay, err := h.store.Create(r.Context(), p.AccountID, p.Subject, key, spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, app)
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

func (h *Handler) validateSource(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// A bounded local temporary file permits zip.ReaderAt without holding the
	// entire upload in memory. The file is removed before the response.
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
	// Accept a single file only; extra parts could hide payloads outside the validated ZIP.
	if next, nextErr := reader.NextPart(); nextErr != io.EOF || next != nil {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "Upload exactly one ZIP file.")
		return
	}
	if err := source.ValidateZIP(f, size); err != nil {
		if errors.Is(err, source.ErrInvalidArchive) {
			if auditErr := h.store.RecordSourceValidation(r.Context(), p.AccountID, p.Subject, size, "Failed", "INVALID_SOURCE_ARCHIVE"); auditErr != nil {
				writeStoreError(w, auditErr)
				return
			}
			writeError(w, http.StatusBadRequest, "INVALID_SOURCE_ARCHIVE", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not inspect the source file.")
		return
	}
	if err := h.store.RecordSourceValidation(r.Context(), p.AccountID, p.Subject, size, "Succeeded", ""); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "sizeBytes": size, "message": "ZIP validated. Build execution is not yet connected."})
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := h.authenticate(r)
	if err == nil && p.AccountID != "" {
		return p, true
	}
	if errors.Is(err, ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The service is temporarily unavailable.")
		return Principal{}, false
	}
	if errors.Is(err, ErrDisabled) {
		writeError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "This account is disabled.")
		return Principal{}, false
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Sign in to continue.")
	return Principal{}, false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": &domain.Error{Code: code, Message: message}})
}
