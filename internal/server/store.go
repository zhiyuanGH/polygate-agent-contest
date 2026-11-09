package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	"polygate/internal/domain"
	"polygate/internal/registry"
)

var (
	ErrNotFound            = errors.New("resource not found")
	ErrDisabled            = errors.New("account disabled")
	ErrImageDeployDisabled = errors.New("approved image deployment disabled for account")
	ErrVolumeDisabled      = errors.New("persistent volumes disabled for account")
	ErrUnavailable         = errors.New("store unavailable")
	ErrForbidden           = errors.New("operation forbidden")
)

type Principal struct {
	AccountID   string
	Subject     string
	DisplayName string
	Role        string
}

type Account struct {
	ID                 string
	DisplayName        string
	Role               string
	Quota              domain.Quota
	ImageDeployEnabled bool
	SourceBuildEnabled bool
	VolumeEnabled      bool
}

type AdminAccount struct {
	ID                 string       `json:"id"`
	Username           string       `json:"username"`
	DisplayName        string       `json:"displayName"`
	Role               string       `json:"role"`
	Enabled            bool         `json:"enabled"`
	Quota              domain.Quota `json:"quota"`
	ImageDeployEnabled bool         `json:"imageDeployEnabled"`
	SourceBuildEnabled bool         `json:"sourceBuildEnabled"`
	VolumeEnabled      bool         `json:"volumeEnabled"`
	AppCount           int          `json:"appCount"`
	CreatedAt          time.Time    `json:"createdAt"`
}

type AdminAccountAudit struct {
	ID              string    `json:"id"`
	ActorAccountID  string    `json:"actorAccountId"`
	ActorUsername   string    `json:"actorUsername"`
	TargetAccountID string    `json:"targetAccountId"`
	Action          string    `json:"action"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"createdAt"`
}

type AdminAccountStore interface {
	ListAdminAccounts(context.Context, string) ([]AdminAccount, error)
	SetAdminAccountEnabled(context.Context, string, string, string, bool, string) (AdminAccount, error)
	ListAdminAccountAudit(context.Context, string, int) ([]AdminAccountAudit, error)
}

type QuotaChange struct {
	ID              string       `json:"id"`
	ActorAccountID  string       `json:"actorAccountId"`
	TargetAccountID string       `json:"targetAccountId"`
	Quota           domain.Quota `json:"quota"`
	State           string       `json:"state"`
	Reason          string       `json:"reason"`
	LastError       string       `json:"lastError,omitempty"`
	CreatedAt       time.Time    `json:"createdAt"`
	AppliedAt       *time.Time   `json:"appliedAt,omitempty"`
}

type AdminQuotaPreviewApp struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Status    string           `json:"status"`
	Resources domain.Resources `json:"resources"`
}

type AdminQuotaPreview struct {
	TargetAccountID string                 `json:"targetAccountId"`
	CurrentQuota    domain.Quota           `json:"currentQuota"`
	ProposedQuota   domain.Quota           `json:"proposedQuota"`
	Configured      domain.Usage           `json:"configured"`
	OverQuota       []string               `json:"overQuota"`
	Apps            []AdminQuotaPreviewApp `json:"apps"`
	CheckedAt       time.Time              `json:"checkedAt"`
}

type AdminQuotaStore interface {
	QueueAdminQuotaChange(context.Context, string, string, string, string, string, domain.Quota) (QuotaChange, bool, error)
	ListAdminQuotaChanges(context.Context, string) ([]QuotaChange, error)
}

type Operation struct {
	ID        string          `json:"id"`
	AppID     string          `json:"appId,omitempty"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"createdAt"`
}

type OperationPage struct {
	Items      []Operation `json:"items"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

type OperationHistoryStore interface {
	ListOperationHistory(context.Context, string, string) (OperationPage, error)
}

type DeploymentStatus struct {
	AppID     string `json:"appId"`
	Status    string `json:"status"`
	TaskID    string `json:"taskId,omitempty"`
	TaskState string `json:"taskState,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}

type DeploymentTask struct {
	ID           string    `json:"id"`
	OperationID  string    `json:"operationId"`
	AccountID    string    `json:"accountId"`
	AppID        string    `json:"appId"`
	RevisionID   string    `json:"revisionId"`
	DesiredState string    `json:"desiredState"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"createdAt"`
}

type ImageArtifact struct {
	ID                 string            `json:"id"`
	SubmittedReference string            `json:"submittedReference"`
	PinnedReference    string            `json:"pinnedReference"`
	PlatformDigest     string            `json:"platformDigest"`
	RegistryHost       string            `json:"registryHost"`
	ConfiguredUser     string            `json:"configuredUser"`
	ScanStatus         string            `json:"scanStatus"`
	ScanTaskState      string            `json:"scanTaskState,omitempty"`
	ScanErrorCode      string            `json:"scanErrorCode,omitempty"`
	ScanSummary        *ImageScanSummary `json:"scanSummary,omitempty"`
	CreatedAt          time.Time         `json:"createdAt"`
}

type ImageScanSummary struct {
	Total         int            `json:"total"`
	Fixable       int            `json:"fixable"`
	Severities    map[string]int `json:"severities"`
	PolicyVersion string         `json:"policyVersion"`
}

type SourceBuildTask struct {
	ID                    string     `json:"id"`
	ArtifactID            string     `json:"artifactId"`
	AccountID             string     `json:"-"`
	SourceSHA256          string     `json:"sourceSha256"`
	SizeBytes             int64      `json:"sizeBytes"`
	State                 string     `json:"state"`
	Attempt               int        `json:"attempt"`
	ErrorCode             string     `json:"errorCode,omitempty"`
	ResultImageArtifactID string     `json:"resultImageArtifactId,omitempty"`
	ResultImageDeletedAt  *time.Time `json:"resultImageDeletedAt,omitempty"`
	CreatedAt             time.Time  `json:"createdAt"`
}

type Volume struct {
	ID        string            `json:"id"`
	Spec      domain.VolumeSpec `json:"spec"`
	State     string            `json:"state"`
	CreatedAt time.Time         `json:"createdAt"`
}

type Revision struct {
	ID              string         `json:"id"`
	Number          int            `json:"number"`
	Spec            domain.AppSpec `json:"spec"`
	ImageDigest     string         `json:"imageDigest"`
	SourceKind      string         `json:"sourceKind"`
	ImageArtifactID string         `json:"imageArtifactId,omitempty"`
	Successful      bool           `json:"successful"`
	Current         bool           `json:"current"`
	CreatedAt       time.Time      `json:"createdAt"`
}

type RevisionPage struct {
	Items              []Revision `json:"items"`
	NextBeforeRevision int        `json:"nextBeforeRevision,omitempty"`
}

type RevisionStore interface {
	ListRevisions(context.Context, string, string, int, int) (RevisionPage, error)
}

type VolumeStore interface {
	ListVolumes(context.Context, string) ([]Volume, error)
	GetVolume(context.Context, string, string) (Volume, error)
	CreateVolume(context.Context, string, string, string, domain.VolumeSpec) (Volume, bool, error)
	ExpandVolume(context.Context, string, string, string, int) (Volume, error)
	DeleteVolume(context.Context, string, string, string, string) (Volume, error)
}

type SourceBuildStore interface {
	ReserveSourceBuild(context.Context, string, string, string, int64) (SourceBuildTask, error)
	ActivateSourceBuild(context.Context, string, string, string) (SourceBuildTask, error)
	AbandonSourceBuild(context.Context, string, string) (bool, error)
	ListSourceBuilds(context.Context, string) ([]SourceBuildTask, error)
	GetSourceBuild(context.Context, string, string) (SourceBuildTask, error)
}

type ImageArtifactStore interface {
	SaveImageArtifact(context.Context, string, string, registry.Image) (ImageArtifact, bool, error)
	ListImageArtifacts(context.Context, string) ([]ImageArtifact, error)
}

type ImageScanReportPointer struct {
	HarborReference string
	ReportID        string
	ReportSHA256    string
	ScanStatus      string
	ReportBody      []byte
	BodySHA256      string
}

type ImageScanReportStore interface {
	GetImageScanReportPointer(context.Context, string, string) (ImageScanReportPointer, error)
}

type ImageScanReportArchiver interface {
	ArchiveImageScanReport(context.Context, string, string, string, string, []byte) error
}

type ImageDeletionStore interface {
	RequestImageDeletion(context.Context, string, string, string) error
}

type Store interface {
	Account(context.Context, string) (Account, error)
	List(context.Context, string) ([]App, domain.Usage, error)
	Get(context.Context, string, string) (App, error)
	DeploymentStatus(context.Context, string, string) (DeploymentStatus, error)
	Create(context.Context, string, string, string, domain.AppSpec) (App, bool, error)
	Delete(context.Context, string, string, string) error
	ListOperations(context.Context, string) ([]Operation, error)
	RecordSourceValidation(context.Context, string, string, int64, string, string) error
}

type AppResourceEditor interface {
	UpdateAppResources(context.Context, string, string, string, domain.Resources) (App, error)
}

type AppRuntimeEditor interface {
	UpdateAppRuntime(context.Context, string, string, string, int, string) (App, error)
}

type RunningResourceUpdateStore interface {
	QueueRunningResourceUpdate(context.Context, string, string, string, string,
		domain.Resources, string) (DeploymentTask, bool, error)
}

type RunningRestartStore interface {
	QueueRunningRestart(context.Context, string, string, string, string,
		string) (DeploymentTask, bool, error)
}

type TrustedDeployStore interface {
	QueueTrustedDeploy(context.Context, string, string, string, string, string, int) (DeploymentTask, bool, error)
}

type StopStore interface {
	QueueStop(context.Context, string, string, string, string) (DeploymentTask, bool, error)
}

type ApprovedImageDeployStore interface {
	QueueApprovedImageDeploy(context.Context, string, string, string, string, string) (DeploymentTask, bool, error)
}

type ApprovedImageUpdateStore interface {
	QueueApprovedImageUpdate(context.Context, string, string, string, string, string) (DeploymentTask, bool, error)
}

type RevisionRollbackStore interface {
	QueueApprovedRevisionRollback(context.Context, string, string, string, string, string) (DeploymentTask, bool, error)
}

func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	if e := domain.AsDomainError(err); e != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": e})
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
		return
	}
	if errors.Is(err, ErrDisabled) {
		writeError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "This account is disabled.")
		return
	}
	if errors.Is(err, ErrForbidden) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "This action is not permitted.")
		return
	}
	if errors.Is(err, ErrImageDeployDisabled) {
		writeError(w, http.StatusForbidden, "IMAGE_DEPLOYMENT_DISABLED",
			"Approved image deployment is not enabled for this account.")
		return
	}
	if errors.Is(err, ErrVolumeDisabled) {
		writeError(w, http.StatusForbidden, "VOLUME_PILOT_DISABLED", "Volumes are not enabled for this account.")
		return
	}
	if errors.Is(err, ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The service is temporarily unavailable.")
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
}

// MemoryStore remains a loopback development adapter. All data is scoped by
// account ID so the same server tests can exercise cross-account behavior.
type MemoryStore struct {
	mu         sync.Mutex
	secretMu   sync.Mutex
	apps       map[string]map[string]App
	keys       map[string]map[string]string
	operations map[string][]Operation
	volumes    map[string]map[string]Volume
	volumeKeys map[string]map[string]string
}

func (s *MemoryStore) CreateWithSecretCheck(ctx context.Context, id, subject, key string,
	spec domain.AppSpec, check func(context.Context) error) (App, bool, error) {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if err := check(ctx); err != nil {
		return App{}, false, err
	}
	return s.Create(ctx, id, subject, key, spec)
}

func (s *MemoryStore) PutSecretWithAudit(ctx context.Context, accountID, subject, key string,
	put func(context.Context, string) error) error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if err := put(ctx, "memory-operation"); err != nil {
		return err
	}
	s.mu.Lock()
	s.addOperationLocked(accountID, "", "SECRET_UPSERT", "Succeeded", map[string]any{"actor": subject, "name": key})
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) DeleteSecretIfUnused(ctx context.Context, accountID, subject, key string,
	remove func(context.Context, string) error) error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	apps, _, err := s.List(ctx, accountID)
	if err != nil {
		return err
	}
	for _, app := range apps {
		for _, selected := range app.Spec.SecretKeys {
			if selected == key {
				return ErrSecretInUse
			}
		}
	}
	if err := remove(ctx, "memory-operation"); err != nil {
		return err
	}
	s.mu.Lock()
	s.addOperationLocked(accountID, "", "SECRET_DELETE", "Succeeded", map[string]any{"actor": subject, "name": key})
	s.mu.Unlock()
	return nil
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{apps: map[string]map[string]App{}, keys: map[string]map[string]string{}, operations: map[string][]Operation{}, volumes: map[string]map[string]Volume{}, volumeKeys: map[string]map[string]string{}}
}

func (s *MemoryStore) ListVolumes(_ context.Context, accountID string) ([]Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Volume, 0, len(s.volumes[accountID]))
	for _, volume := range s.volumes[accountID] {
		items = append(items, volume)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items, nil
}

func (s *MemoryStore) GetVolume(_ context.Context, accountID, volumeID string) (Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	volume, ok := s.volumes[accountID][volumeID]
	if !ok {
		return Volume{}, ErrNotFound
	}
	return volume, nil
}

func (s *MemoryStore) CreateVolume(_ context.Context, accountID, subject, key string, spec domain.VolumeSpec) (Volume, bool, error) {
	if err := domain.ValidateVolumeSpec(spec); err != nil {
		return Volume{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.volumeKeys[accountID][key]; ok {
		previous := s.volumes[accountID][id]
		if previous.Spec != spec {
			return Volume{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used with different settings."}
		}
		return previous, true, nil
	}
	used := 0
	for _, volume := range s.volumes[accountID] {
		if volume.Spec.Name == spec.Name {
			return Volume{}, false, &domain.Error{Code: "VOLUME_NAME_TAKEN", Field: "name", Message: "A volume with this name already exists."}
		}
		used += volume.Spec.SizeGiB
	}
	if len(s.volumes[accountID]) >= domain.StudentVolumeCountLimit || used+spec.SizeGiB > domain.StudentVolumeGiBLimit {
		return Volume{}, false, &domain.Error{Code: "VOLUME_QUOTA_EXCEEDED", Message: "Volumes exceed the account limit of two claims and 10 GiB."}
	}
	id, err := NewID()
	if err != nil {
		return Volume{}, false, err
	}
	volume := Volume{ID: id, Spec: spec, State: "Reserved", CreatedAt: time.Now().UTC()}
	if s.volumes[accountID] == nil {
		s.volumes[accountID] = map[string]Volume{}
	}
	if s.volumeKeys[accountID] == nil {
		s.volumeKeys[accountID] = map[string]string{}
	}
	s.volumes[accountID][id] = volume
	s.volumeKeys[accountID][key] = id
	s.addOperationLocked(accountID, "", "CREATE_VOLUME_RESERVATION", "Succeeded", map[string]any{"actor": subject, "volumeId": id, "sizeGiB": spec.SizeGiB})
	return volume, false, nil
}

func (s *MemoryStore) ExpandVolume(_ context.Context, accountID, subject, volumeID string, sizeGiB int) (Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.volumes[accountID][volumeID]
	if !ok {
		return Volume{}, ErrNotFound
	}
	if sizeGiB < 1 || sizeGiB > domain.StudentVolumeGiBLimit || sizeGiB < item.Spec.SizeGiB {
		return Volume{}, &domain.Error{Code: "INVALID_VOLUME_SIZE", Field: "sizeGiB", Message: "Volumes may only expand up to 10 GiB."}
	}
	if sizeGiB == item.Spec.SizeGiB {
		return item, nil
	}
	if item.State != "Ready" {
		return Volume{}, &domain.Error{Code: "VOLUME_NOT_READY", Message: "Wait until this volume is ready before expanding it."}
	}
	used := 0
	for _, volume := range s.volumes[accountID] {
		used += volume.Spec.SizeGiB
	}
	if used+sizeGiB-item.Spec.SizeGiB > domain.StudentVolumeGiBLimit {
		return Volume{}, &domain.Error{Code: "VOLUME_QUOTA_EXCEEDED", Message: "Volumes exceed the account limit of 10 GiB."}
	}
	item.Spec.SizeGiB = sizeGiB
	item.State = "Expanding"
	s.volumes[accountID][volumeID] = item
	s.addOperationLocked(accountID, "", "EXPAND_VOLUME", "Pending", map[string]any{"actor": subject, "volumeId": volumeID, "sizeGiB": sizeGiB})
	return item, nil
}

func (s *MemoryStore) DeleteVolume(_ context.Context, accountID, subject, volumeID, confirmName string) (Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.volumes[accountID][volumeID]
	if !ok {
		return Volume{}, ErrNotFound
	}
	if confirmName != item.Spec.Name {
		return Volume{}, &domain.Error{Code: "VOLUME_CONFIRMATION_MISMATCH", Field: "confirmName", Message: "Type the volume name to confirm permanent data deletion."}
	}
	if item.State == "Deleting" {
		return item, nil
	}
	if item.State != "Ready" {
		return Volume{}, &domain.Error{Code: "VOLUME_NOT_READY", Message: "Only a ready, unbound volume can be deleted."}
	}
	for _, app := range s.apps[accountID] {
		if app.Spec.VolumeID == volumeID {
			return Volume{}, &domain.Error{Code: "VOLUME_ALREADY_BOUND", Message: "Delete the app before deleting its volume."}
		}
	}
	item.State = "Deleting"
	s.volumes[accountID][volumeID] = item
	s.addOperationLocked(accountID, "", "DELETE_VOLUME", "Pending", map[string]any{"actor": subject, "volumeId": volumeID})
	return item, nil
}

func (s *MemoryStore) Account(_ context.Context, id string) (Account, error) {
	name := "Test Student"
	if id == "local-student" {
		name = "Local Student"
	}
	return Account{ID: id, DisplayName: name, Role: "student", Quota: domain.StudentQuota, VolumeEnabled: true}, nil
}

func (s *MemoryStore) List(_ context.Context, id string) ([]App, domain.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apps, usage := s.listLocked(id)
	return apps, usage, nil
}

func (s *MemoryStore) listLocked(id string) ([]App, domain.Usage) {
	items := make([]App, 0, len(s.apps[id]))
	var usage domain.Usage
	for _, app := range s.apps[id] {
		items = append(items, app)
		usage = usage.Add(app.Spec.Resources.Usage())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items, usage
}

func (s *MemoryStore) Get(_ context.Context, id, appID string) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[id][appID]
	if !ok {
		return App{}, ErrNotFound
	}
	return app, nil
}

func (s *MemoryStore) UpdateAppResources(_ context.Context, id, subject, appID string, resources domain.Resources) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[id][appID]
	if !ok {
		return App{}, ErrNotFound
	}
	if app.Status != "Draft" && app.Status != "Stopped" {
		return App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its resource settings."}
	}
	updated := app.Spec
	updated.Resources = resources
	if err := domain.ValidateSpec(updated); err != nil {
		return App{}, err
	}
	if app.Spec.Resources == resources {
		return app, nil
	}
	_, used := s.listLocked(id)
	if err := domain.CheckAdmission(used.Sub(app.Spec.Resources.Usage()), domain.Usage{},
		resources.Usage(), domain.StudentQuota, domain.Quota{}); err != nil {
		return App{}, err
	}
	before := app.Spec.Resources
	app.Spec.Resources = resources
	s.apps[id][appID] = app
	s.addOperationLocked(id, appID, "UPDATE_APP_RESOURCES", "Succeeded",
		map[string]any{"actor": subject, "before": before, "after": resources})
	return app, nil
}

func (s *MemoryStore) UpdateAppRuntime(_ context.Context, id, subject, appID string, port int, healthPath string) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[id][appID]
	if !ok {
		return App{}, ErrNotFound
	}
	if app.Status != "Draft" && app.Status != "Stopped" && app.Status != "Failed" {
		return App{}, &domain.Error{Code: "APP_NOT_EDITABLE", Message: "Stop this app before changing its port or health path."}
	}
	updated := app.Spec
	updated.Port = port
	updated.HealthPath = healthPath
	if err := domain.ValidateSpec(updated); err != nil {
		return App{}, err
	}
	if app.Spec.Port == port && app.Spec.HealthPath == healthPath {
		return app, nil
	}
	before := map[string]any{"port": app.Spec.Port, "healthPath": app.Spec.HealthPath}
	app.Spec.Port = port
	app.Spec.HealthPath = healthPath
	s.apps[id][appID] = app
	s.addOperationLocked(id, appID, "UPDATE_APP_RUNTIME", "Succeeded",
		map[string]any{"actor": subject, "before": before,
			"after": map[string]any{"port": port, "healthPath": healthPath}})
	return app, nil
}

func (s *MemoryStore) ListRevisions(_ context.Context, accountID, appID string, _, _ int) (RevisionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[accountID][appID]; !ok {
		return RevisionPage{}, ErrNotFound
	}
	return RevisionPage{Items: []Revision{}}, nil
}

func (s *MemoryStore) DeploymentStatus(ctx context.Context, id, appID string) (DeploymentStatus, error) {
	app, err := s.Get(ctx, id, appID)
	if err != nil {
		return DeploymentStatus{}, err
	}
	return DeploymentStatus{AppID: app.ID, Status: app.Status}, nil
}

func (s *MemoryStore) Create(_ context.Context, id, subject, key string, spec domain.AppSpec) (App, bool, error) {
	if err := domain.ValidateSpec(spec); err != nil {
		return App{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prevID, ok := s.keys[id][key]; ok {
		previous := s.apps[id][prevID]
		if !reflect.DeepEqual(previous.Spec, spec) {
			return App{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used with different settings."}
		}
		return previous, true, nil
	}
	if spec.Persistent {
		volume, ok := s.volumes[id][spec.VolumeID]
		if !ok || volume.State != "Ready" {
			return App{}, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Select a ready volume from this account."}
		}
		for _, app := range s.apps[id] {
			if app.Spec.VolumeID == spec.VolumeID {
				return App{}, false, &domain.Error{Code: "VOLUME_ALREADY_BOUND", Field: "volumeId", Message: "This volume is already bound to an app."}
			}
		}
	}
	_, usage := s.listLocked(id)
	for _, app := range s.apps[id] {
		if app.Spec.Name == spec.Name {
			return App{}, false, &domain.Error{Code: "APP_NAME_TAKEN", Field: "name", Message: "An app with this name already exists."}
		}
	}
	if err := domain.CheckAdmission(usage, domain.Usage{}, spec.Resources.Usage(), domain.StudentQuota, domain.Quota{}); err != nil {
		return App{}, false, err
	}
	appID, err := NewID()
	if err != nil {
		return App{}, false, err
	}
	app := App{ID: appID, Spec: spec, Status: "Draft", CreatedAt: time.Now().UTC()}
	if s.apps[id] == nil {
		s.apps[id] = map[string]App{}
	}
	if s.keys[id] == nil {
		s.keys[id] = map[string]string{}
	}
	s.apps[id][appID] = app
	s.keys[id][key] = appID
	s.addOperationLocked(id, appID, "CREATE_DRAFT", "Succeeded", map[string]any{"actor": subject})
	return app, false, nil
}

func (s *MemoryStore) Delete(_ context.Context, id, subject, appID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id][appID]; !ok {
		return ErrNotFound
	}
	delete(s.apps[id], appID)
	for key, value := range s.keys[id] {
		if value == appID {
			delete(s.keys[id], key)
		}
	}
	s.addOperationLocked(id, appID, "DELETE_DRAFT", "Succeeded", map[string]any{"actor": subject, "appId": appID})
	return nil
}

func (s *MemoryStore) addOperationLocked(id, appID, kind, state string, detail map[string]any) {
	opID, _ := NewID()
	data, _ := json.Marshal(detail)
	s.operations[id] = append(s.operations[id], Operation{ID: opID, AppID: appID, Kind: kind, State: state, Detail: data, CreatedAt: time.Now().UTC()})
}

func (s *MemoryStore) ListOperations(_ context.Context, id string) ([]Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := s.operations[id]
	items := make([]Operation, 0, min(len(ops), 50))
	for i := len(ops) - 1; i >= 0 && len(items) < 50; i-- {
		items = append(items, ops[i])
	}
	return items, nil
}

func (s *MemoryStore) RecordSourceValidation(_ context.Context, id, subject string, size int64, state, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addOperationLocked(id, "", "VALIDATE_ZIP", state, map[string]any{"actor": subject, "sizeBytes": size, "errorCode": code})
	return nil
}
