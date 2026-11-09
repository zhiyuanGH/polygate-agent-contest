package domain

import (
	"path"
	"regexp"
	"strings"
)

var volumeIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var mountPathPattern = regexp.MustCompile(`^/[A-Za-z0-9/_-]+$`)

const (
	StudentVolumeCountLimit = 2
	StudentVolumeGiBLimit   = 10
)

type VolumeSpec struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"sizeGiB"`
}

func ValidateVolumeSpec(spec VolumeSpec) error {
	if !appNamePattern.MatchString(spec.Name) || strings.Contains(spec.Name, "--") {
		return &Error{Code: "INVALID_VOLUME_NAME", Field: "name", Message: "Use 1–40 lowercase letters, numbers or hyphens; start with a letter."}
	}
	if spec.SizeGiB < 1 || spec.SizeGiB > StudentVolumeGiBLimit {
		return &Error{Code: "INVALID_VOLUME_SIZE", Field: "sizeGiB", Message: "Volume size must be between 1 and 10 GiB."}
	}
	return nil
}

func ValidateVolumeBinding(persistent bool, volumeID, mountPath string) error {
	if !persistent {
		if volumeID != "" || mountPath != "" {
			return &Error{Code: "INVALID_VOLUME_BINDING", Field: "volumeId", Message: "Stateless apps cannot bind a volume."}
		}
		return nil
	}
	if !volumeIDPattern.MatchString(volumeID) {
		return &Error{Code: "INVALID_VOLUME_BINDING", Field: "volumeId", Message: "Select a ready volume from this account."}
	}
	if len(mountPath) > 128 || !mountPathPattern.MatchString(mountPath) ||
		path.Clean(mountPath) != mountPath || mountPath == "/" {
		return &Error{Code: "INVALID_MOUNT_PATH", Field: "mountPath", Message: "Use a clean absolute mount path."}
	}
	for _, blocked := range []string{"/proc", "/sys", "/dev", "/tmp", "/run", "/var/run", "/var/run/secrets"} {
		if mountPath == blocked || strings.HasPrefix(mountPath, blocked+"/") {
			return &Error{Code: "INVALID_MOUNT_PATH", Field: "mountPath", Message: "Choose a path outside system and temporary directories."}
		}
	}
	return nil
}
