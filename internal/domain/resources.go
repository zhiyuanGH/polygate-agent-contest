package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Resources are expressed in millicores and MiB. Using integers avoids
// disagreements between the UI, admission checks and Kubernetes quantities.
type Resources struct {
	CPURequestMilli int64 `json:"cpuRequestMilli"`
	CPULimitMilli   int64 `json:"cpuLimitMilli"`
	MemoryMiB       int64 `json:"memoryMiB"`
	Replicas        int64 `json:"replicas"`
}

type Usage struct {
	CPURequestMilli int64 `json:"cpuRequestMilli"`
	CPULimitMilli   int64 `json:"cpuLimitMilli"`
	MemoryMiB       int64 `json:"memoryMiB"`
	Pods            int64 `json:"pods"`
	Apps            int64 `json:"apps"`
}

type Quota struct {
	CPURequestMilli int64 `json:"cpuRequestMilli"`
	CPULimitMilli   int64 `json:"cpuLimitMilli"`
	MemoryMiB       int64 `json:"memoryMiB"`
	Pods            int64 `json:"pods"`
	Apps            int64 `json:"apps"`
}

var StudentQuota = Quota{CPURequestMilli: 2000, CPULimitMilli: 4000, MemoryMiB: 4096, Pods: 4, Apps: 2}
var GlobalQuota = Quota{CPURequestMilli: 60000, CPULimitMilli: 80000, MemoryMiB: 102400, Pods: 60}

// Administrator changes remain bounded by the pilot's physical capacity and
// tenant template. A single container still has its separate 2 CPU / 4 GiB cap.
func ValidateAccountQuota(q Quota) error {
	if q.CPURequestMilli < 250 || q.CPURequestMilli > 8000 ||
		q.CPULimitMilli < 1000 || q.CPULimitMilli > 16000 ||
		q.CPURequestMilli > q.CPULimitMilli ||
		q.MemoryMiB < 1024 || q.MemoryMiB > 16384 ||
		q.Pods < 1 || q.Pods > 8 || q.Apps < 1 || q.Apps > 4 {
		return &Error{Code: "INVALID_ACCOUNT_QUOTA", Field: "quota",
			Message: "Account quota must be within 250–8000m CPU request, 1000–16000m CPU limit, 1–16 GiB memory, 1–8 Pods and 1–4 apps."}
	}
	return nil
}

var appNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$|^[a-z]$`)

type AppSpec struct {
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Port       int       `json:"port"`
	HealthPath string    `json:"healthPath,omitempty"`
	Resources  Resources `json:"resources"`
	Persistent bool      `json:"persistent"`
	VolumeID   string    `json:"volumeId,omitempty"`
	MountPath  string    `json:"mountPath,omitempty"`
	SecretKeys []string  `json:"secretKeys,omitempty"`
}

var secretKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func ValidateSecretKeys(keys []string) error {
	if len(keys) > 8 {
		return &Error{Code: "INVALID_SECRET_KEYS", Field: "secretKeys", Message: "Select at most eight environment secrets."}
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !secretKeyPattern.MatchString(key) || strings.HasPrefix(key, "POLYGATE_") ||
			strings.HasPrefix(key, "KUBERNETES_") || seen[key] {
			return &Error{Code: "INVALID_SECRET_KEYS", Field: "secretKeys", Message: "Use distinct uppercase environment variable names without reserved prefixes."}
		}
		seen[key] = true
	}
	return nil
}

type Error struct {
	Code      string `json:"code"`
	Field     string `json:"field,omitempty"`
	Message   string `json:"message"`
	Current   int64  `json:"current,omitempty"`
	Allowed   int64  `json:"allowed,omitempty"`
	Requested int64  `json:"requested,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func ValidateSpec(s AppSpec) error {
	if !appNamePattern.MatchString(s.Name) || strings.Contains(s.Name, "--") {
		return &Error{Code: "INVALID_APP_NAME", Field: "name", Message: "Use 1–40 lowercase letters, numbers or hyphens; start with a letter."}
	}
	if s.Image == "" || strings.ContainsAny(s.Image, " \r\n\t") || len(s.Image) > 512 {
		return &Error{Code: "INVALID_IMAGE", Field: "image", Message: "Enter a valid container image reference."}
	}
	if s.Port < 1 || s.Port > 65535 {
		return &Error{Code: "INVALID_PORT", Field: "port", Message: "Port must be between 1 and 65535."}
	}
	if s.HealthPath != "" {
		if len(s.HealthPath) > 128 || !strings.HasPrefix(s.HealthPath, "/") ||
			strings.HasPrefix(s.HealthPath, "//") || strings.Contains(s.HealthPath, "..") ||
			strings.ContainsAny(s.HealthPath, "?#%\\\r\n\t ") {
			return &Error{Code: "INVALID_HEALTH_PATH", Field: "healthPath", Message: "Use an absolute HTTP path up to 128 characters without spaces, query or fragment."}
		}
		for _, c := range s.HealthPath {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || c == '/' || c == '-' || c == '_' || c == '.' || c == '~') {
				return &Error{Code: "INVALID_HEALTH_PATH", Field: "healthPath", Message: "Use letters, digits, /, -, _, . or ~ in the HTTP health path."}
			}
		}
	}
	r := s.Resources
	if r.CPURequestMilli < 100 || r.CPURequestMilli > 2000 || r.CPULimitMilli < r.CPURequestMilli || r.CPULimitMilli > 2000 {
		return &Error{Code: "INVALID_CPU", Field: "resources.cpu", Message: "CPU request must be 100–2000m; limit must be at least the request and at most 2000m."}
	}
	if r.MemoryMiB < 128 || r.MemoryMiB > 4096 {
		return &Error{Code: "INVALID_MEMORY", Field: "resources.memoryMiB", Message: "Memory must be between 128MiB and 4096MiB."}
	}
	if r.Replicas < 1 || r.Replicas > 2 || (s.Persistent && r.Replicas != 1) {
		return &Error{Code: "INVALID_REPLICAS", Field: "resources.replicas", Message: "Use one replica for persistent apps; stateless apps may use one or two."}
	}
	if err := ValidateVolumeBinding(s.Persistent, s.VolumeID, s.MountPath); err != nil {
		return err
	}
	if err := ValidateSecretKeys(s.SecretKeys); err != nil {
		return err
	}
	return nil
}

func (r Resources) Usage() Usage {
	return Usage{CPURequestMilli: r.CPURequestMilli * r.Replicas, CPULimitMilli: r.CPULimitMilli * r.Replicas, MemoryMiB: r.MemoryMiB * r.Replicas, Pods: r.Replicas, Apps: 1}
}

func (u Usage) Add(v Usage) Usage {
	return Usage{u.CPURequestMilli + v.CPURequestMilli, u.CPULimitMilli + v.CPULimitMilli, u.MemoryMiB + v.MemoryMiB, u.Pods + v.Pods, u.Apps + v.Apps}
}

func (u Usage) Sub(v Usage) Usage {
	return Usage{u.CPURequestMilli - v.CPURequestMilli, u.CPULimitMilli - v.CPULimitMilli, u.MemoryMiB - v.MemoryMiB, u.Pods - v.Pods, u.Apps - v.Apps}
}

// CheckAdmission checks both account and global limits. The caller must hold
// database locks for both budget rows while calculating and reserving usage.
func CheckAdmission(account Usage, global Usage, delta Usage, accountQuota, globalQuota Quota) error {
	if err := check(account, delta, accountQuota, "QUOTA_EXCEEDED"); err != nil {
		return err
	}
	return check(global, delta, globalQuota, "GLOBAL_CAPACITY_EXCEEDED")
}

func check(current, delta Usage, q Quota, code string) error {
	values := []struct {
		name              string
		current, add, max int64
	}{
		{"cpuRequestMilli", current.CPURequestMilli, delta.CPURequestMilli, q.CPURequestMilli},
		{"cpuLimitMilli", current.CPULimitMilli, delta.CPULimitMilli, q.CPULimitMilli},
		{"memoryMiB", current.MemoryMiB, delta.MemoryMiB, q.MemoryMiB},
		{"pods", current.Pods, delta.Pods, q.Pods},
		{"apps", current.Apps, delta.Apps, q.Apps},
	}
	for _, v := range values {
		if v.max > 0 && (v.add > v.max-v.current) {
			return &Error{Code: code, Field: v.name, Message: fmt.Sprintf("%s exceeds the allowed capacity.", v.name), Current: v.current, Requested: v.add, Allowed: v.max}
		}
	}
	return nil
}

func AsDomainError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
