package source

import (
	"errors"
	"os"
	"path/filepath"
)

// BuildLogPath uses only a validated task UUID; logs share the source PVC so
// the API can read output while the worker is writing it.
func BuildLogPath(root, taskID string) (string, error) {
	if !archiveID.MatchString(taskID) || !filepath.IsAbs(root) {
		return "", errors.New("invalid build log path")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("build log root must be a real directory")
	}
	return filepath.Join(root, taskID+".log"), nil
}
