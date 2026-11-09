package sourcebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"polygate/internal/buildagent"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/server"
	"polygate/internal/source"
)

const Lease = 20 * time.Minute
const WorkTimeout = 18 * time.Minute

var errSourceInvalid = errors.New("source archive is invalid")

type TaskStore interface {
	ClaimNextSourceBuild(context.Context, string, time.Duration) (postgres.SourceBuildTask, bool, error)
	FailSourceBuild(context.Context, string, string, int, string, bool) error
	CompleteLeasedSourceBuild(context.Context, postgres.SourceBuildTask, string, registry.Image) (server.ImageArtifact, error)
}

type Agent interface {
	Ready(context.Context) error
	Build(context.Context, buildagent.Request) (buildagent.Result, error)
}

// Verifier must read the image back from the account's private Harbor project
// and return metadata obtained from the registry, not from the build guest.
type Verifier interface {
	EnsureBuildProject(context.Context, string) error
	VerifyBuiltImage(context.Context, string, string, string) (registry.Image, error)
}

type Worker struct {
	Store      TaskStore
	Agent      Agent
	Verifier   Verifier
	SourceRoot string
	ID         string
}

// RunOnce consumes one build. Errors are intentionally reduced to fixed codes:
// an agent or registry error may contain credentials or a signed blob URL.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w == nil || w.Store == nil || w.Agent == nil || w.Verifier == nil ||
		w.ID == "" || !filepath.IsAbs(w.SourceRoot) {
		return false, errors.New("source build worker is not configured")
	}
	if err := w.Agent.Ready(ctx); err != nil {
		return false, errors.New("build agent is not ready")
	}
	task, ok, err := w.Store.ClaimNextSourceBuild(ctx, w.ID, Lease)
	if err != nil || !ok {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, WorkTimeout)
	defer cancel()
	file, err := openVerifiedSource(w.SourceRoot, task)
	if err != nil {
		return true, w.fail(task, "SOURCE_ARCHIVE_INVALID", !errors.Is(err, errSourceInvalid))
	}
	defer file.Close()
	if err := w.Verifier.EnsureBuildProject(workCtx, task.AccountID); err != nil {
		return true, w.fail(task, "BUILD_PROJECT_UNAVAILABLE", true)
	}
	if _, err := source.PruneBuildLogsForBudget(workCtx, w.SourceRoot,
		source.MaxRetainedBuildLogBytes-2*buildagent.MaxBuildLogBytes,
		30*time.Minute, time.Now(), task.ID); err != nil {
		return true, w.fail(task, "BUILD_STORAGE_UNAVAILABLE", true)
	}
	logPath, err := source.BuildLogPath(w.SourceRoot, task.ID)
	if err != nil {
		return true, w.fail(task, "BUILD_STORAGE_UNAVAILABLE", true)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return true, w.fail(task, "BUILD_STORAGE_UNAVAILABLE", true)
	}
	pathInfo, pathErr := os.Lstat(logPath)
	openedInfo, statErr := logFile.Stat()
	if pathErr != nil || statErr != nil || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, openedInfo) || pathInfo.Mode().Perm()&0077 != 0 || openedInfo.Size() > 64<<20 {
		_ = logFile.Close()
		return true, w.fail(task, "BUILD_STORAGE_UNAVAILABLE", true)
	}
	if _, err = fmt.Fprintf(logFile, "\n--- Build attempt %d ---\n", task.Attempt); err != nil {
		_ = logFile.Close()
		return true, w.fail(task, "BUILD_STORAGE_UNAVAILABLE", true)
	}
	result, err := w.Agent.Build(workCtx, buildagent.Request{TaskID: task.ID,
		Attempt: task.Attempt, AccountID: task.AccountID, SourceSHA256: task.SourceSHA256,
		SourceBytes: task.SizeBytes, Source: file, LogWriter: logFile})
	if syncErr := logFile.Sync(); err == nil && syncErr != nil {
		err = syncErr
	}
	if closeErr := logFile.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		var agentErr *buildagent.AgentError
		if errors.As(err, &agentErr) {
			switch agentErr.Code {
			case "BUILD_FAILED", "BUILD_TIMEOUT", "INVALID_SOURCE_ARCHIVE", "SOURCE_MISMATCH":
				return true, w.fail(task, agentErr.Code, false)
			}
		}
		return true, w.fail(task, "BUILD_AGENT_UNAVAILABLE", true)
	}
	if !validDigest(result.ImageDigest) || !validDigest(result.LogSHA256) {
		return true, w.fail(task, "BUILD_RESULT_INVALID", false)
	}
	image, err := w.Verifier.VerifyBuiltImage(workCtx, task.AccountID, task.ID, result.ImageDigest)
	if err != nil {
		return true, w.fail(task, "BUILD_IMAGE_UNVERIFIED", true)
	}
	if image.ManifestDigest != result.ImageDigest || image.PlatformDigest != result.ImageDigest ||
		image.OS != "linux" || image.Architecture != "amd64" ||
		image.RegistryHost != harbor.RegistryHost {
		return true, w.fail(task, "BUILD_IMAGE_MISMATCH", false)
	}
	project, err := harbor.AccountProjectName(task.AccountID)
	if err != nil || image.PinnedReference != harbor.RegistryHost+"/"+project+"/agent@"+result.ImageDigest ||
		image.SubmittedReference != image.PinnedReference {
		return true, w.fail(task, "BUILD_IMAGE_MISMATCH", false)
	}
	_, err = w.Store.CompleteLeasedSourceBuild(workCtx, task, w.ID, image)
	if err != nil {
		if errors.Is(err, server.ErrNotFound) {
			return true, fmt.Errorf("source build task %s lost its lease", task.ID)
		}
		var domainErr *domain.Error
		if errors.As(err, &domainErr) && domainErr.Code == "IMAGE_ARTIFACT_LIMIT" {
			return true, w.fail(task, "IMAGE_ARTIFACT_LIMIT", false)
		}
		if errors.Is(err, server.ErrDisabled) {
			return true, w.fail(task, "ACCOUNT_DISABLED", false)
		}
		return true, w.fail(task, "BUILD_IMAGE_REGISTRATION_FAILED", true)
	}
	return true, nil
}

func openVerifiedSource(root string, task postgres.SourceBuildTask) (*os.File, error) {
	path, err := source.ArchivePath(root, task.ArtifactID)
	if err != nil || task.SizeBytes < 1 || task.SizeBytes > source.MaxArchiveBytes ||
		!validDigest(task.SourceSHA256) {
		return nil, errSourceInvalid
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, errSourceInvalid
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() != task.SizeBytes {
		return nil, errSourceInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*os.File, error) { _ = f.Close(); return nil, err }
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != task.SizeBytes {
		return closeOnError(errSourceInvalid)
	}
	if err = source.ValidateZIP(f, task.SizeBytes); err != nil {
		return closeOnError(errSourceInvalid)
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.NewSectionReader(f, 0, task.SizeBytes)); err != nil ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != task.SourceSHA256 {
		return closeOnError(errSourceInvalid)
	}
	return f, nil
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || value[:7] != "sha256:" {
		return false
	}
	for _, b := range value[7:] {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func (w *Worker) fail(task postgres.SourceBuildTask, code string, retry bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Store.FailSourceBuild(ctx, task.ID, w.ID, task.Attempt, code, retry); err != nil {
		return fmt.Errorf("source build task %s could not record %s: %T", task.ID, code, err)
	}
	return fmt.Errorf("source build task %s: %s", task.ID, code)
}
