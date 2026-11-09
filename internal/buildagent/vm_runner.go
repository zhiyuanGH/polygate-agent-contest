package buildagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const MaxBuildLogBytes int64 = 20 << 20

// VMRunner stages a verified archive for a root-owned, digest-pinned local
// helper. The helper owns the fixed libvirt template, guest execution, image
// export and account-scoped Harbor push. Its result is never trusted without
// the worker reading the task tag and image back from Harbor.
type VMRunner struct {
	Cleaner         LibvirtCleaner
	HelperPath      string
	HelperSHA256    string
	BaseImagePath   string
	BaseImageSHA256 string
	TrustedOwnerUID uint32
	Commands        CommandRunner
}

func (r VMRunner) StopAndClean(ctx context.Context, slot int) error {
	return r.Cleaner.StopAndClean(ctx, slot)
}

func (r VMRunner) Validate() error {
	if r.Commands == nil || r.Cleaner.Commands == nil ||
		!filepath.IsAbs(r.Cleaner.WorkRoot) || !filepath.IsAbs(r.HelperPath) ||
		!filepath.IsAbs(r.BaseImagePath) || !sha256Pattern.MatchString(r.HelperSHA256) ||
		!sha256Pattern.MatchString(r.BaseImageSHA256) {
		return errors.New("incomplete VM runner configuration")
	}
	if err := secureOwnerDirectory(r.Cleaner.WorkRoot); err != nil {
		return err
	}
	if err := securePinnedFile(r.HelperPath, r.HelperSHA256, true, r.TrustedOwnerUID); err != nil {
		return fmt.Errorf("VM helper: %w", err)
	}
	if err := securePinnedFile(r.BaseImagePath, r.BaseImageSHA256, false, r.TrustedOwnerUID); err != nil {
		return fmt.Errorf("VM base image: %w", err)
	}
	return nil
}

func secureOwnerDirectory(path string) error {
	if err := securePathParents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("work directory must be owned by the agent and not group/world writable")
	}
	return nil
}

// A pinned file is only immutable to other host users when every ancestor is
// protected against replacement. A root-owned sticky directory such as /tmp
// is safe for a child owned by the agent during local tests.
func securePathParents(path string) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok ||
			(stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return errors.New("pinned path has an untrusted parent directory")
		}
		if info.Mode().Perm()&0022 != 0 &&
			!(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return errors.New("pinned path has a writable parent directory")
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

func securePinnedFile(path, expected string, executable bool, ownerUID uint32) error {
	if err := securePathParents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != ownerUID ||
		info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 3<<30 {
		return errors.New("pinned file owner, mode or size is invalid")
	}
	if executable && info.Mode().Perm()&0100 == 0 {
		return errors.New("VM helper is not owner-executable")
	}
	if !executable && info.Mode().Perm()&0222 != 0 {
		return errors.New("VM base image must be read-only")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("pinned file changed during validation")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("pinned file SHA-256 differs from configuration")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("pinned file changed during validation")
	}
	return nil
}

func (r VMRunner) Execute(ctx context.Context, slot int, request Request) (Result, error) {
	return executeHelper(ctx, slot, request, r.Validate, r.Cleaner.WorkRoot,
		r.HelperPath, r.Commands, []string{"--base-image", r.BaseImagePath})
}

func executeHelper(ctx context.Context, slot int, request Request, validate func() error,
	workRoot, helperPath string, commands CommandRunner, extraArgs []string) (Result, error) {
	if slot < 0 || slot >= SlotCount || !taskIDPattern.MatchString(request.TaskID) ||
		!taskIDPattern.MatchString(request.AccountID) || request.Attempt < 1 || request.Attempt > 3 ||
		!sha256Pattern.MatchString(request.SourceSHA256) || request.SourceBytes < 1 ||
		request.SourceBytes > 100<<20 || request.Source == nil {
		return Result{}, ErrInvalidTask
	}
	if err := validate(); err != nil {
		return Result{}, err
	}
	dir := filepath.Join(workRoot, fmt.Sprintf("slot-%d", slot))
	if err := os.Mkdir(dir, 0700); err != nil {
		return Result{}, err
	}
	sourcePath := filepath.Join(dir, "source.zip")
	f, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	hash := sha256.New()
	written, writeErr := io.Copy(io.MultiWriter(f, hash), io.NewSectionReader(request.Source, 0, request.SourceBytes))
	if writeErr == nil && written != request.SourceBytes {
		writeErr = errors.New("staged source length differs from signed task")
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return Result{}, writeErr
	}
	if closeErr != nil {
		return Result{}, closeErr
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != request.SourceSHA256 {
		return Result{}, errors.New("staged source digest mismatch")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return Result{}, err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil {
		return Result{}, err
	}
	if closeErr != nil {
		return Result{}, closeErr
	}
	var logDone chan struct{}
	var logResult chan error
	if request.LogWriter != nil {
		logDone = make(chan struct{})
		logResult = make(chan error, 1)
		go func() { logResult <- followVMLog(ctx, filepath.Join(dir, "build.log"), request.LogWriter, logDone) }()
	}
	args := []string{
		"--slot", strconv.Itoa(slot), "--task-id", request.TaskID,
		"--account-id", request.AccountID, "--attempt", strconv.Itoa(request.Attempt),
		"--source", sourcePath}
	args = append(args, extraArgs...)
	args = append(args, "--work-dir", dir, "--result", filepath.Join(dir, "result.json"))
	_, err = commands.Run(ctx, helperPath, args...)
	if logDone != nil {
		close(logDone)
		if logErr := <-logResult; logErr != nil {
			return Result{}, logErr
		}
	}
	if err != nil {
		return Result{}, err
	}
	return readVMResult(dir)
}

// followVMLog copies host-written build output while the helper is running and
// takes one final snapshot before slot cleanup removes the file.
func followVMLog(ctx context.Context, path string, out io.Writer, done <-chan struct{}) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var offset int64
	read := func() error {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < offset || info.Size() > MaxBuildLogBytes {
			return errors.New("VM build log is invalid")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("VM build log changed during read")
		}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		buf := make([]byte, 32<<10)
		for offset < info.Size() {
			n, readErr := f.Read(buf[:min(int64(len(buf)), info.Size()-offset)])
			if n > 0 {
				written, writeErr := out.Write(buf[:n])
				if writeErr != nil {
					return writeErr
				}
				if written != n {
					return io.ErrShortWrite
				}
				offset += int64(n)
			}
			if readErr != nil {
				return readErr
			}
			if n == 0 {
				return io.ErrUnexpectedEOF
			}
		}
		return nil
	}
	for {
		select {
		case <-done:
			return read()
		case <-ctx.Done():
			return read()
		case <-ticker.C:
			if err := read(); err != nil {
				return err
			}
		}
	}
}

func readVMResult(dir string) (Result, error) {
	var result Result
	path := filepath.Join(dir, "result.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 4096 ||
		info.Mode().Perm()&0077 != 0 {
		return result, errors.New("VM helper result is missing or invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!sha256Pattern.MatchString(result.ImageDigest) || !sha256Pattern.MatchString(result.LogSHA256) {
		return Result{}, errors.New("VM helper result fields are invalid")
	}
	logPath := filepath.Join(dir, "build.log")
	logInfo, err := os.Lstat(logPath)
	if err != nil || !logInfo.Mode().IsRegular() || logInfo.Size() > MaxBuildLogBytes ||
		logInfo.Mode().Perm()&0077 != 0 {
		return Result{}, errors.New("VM build log is missing or exceeds its limit")
	}
	logFile, err := os.Open(logPath)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(logFile, MaxBuildLogBytes+1)); err != nil ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != result.LogSHA256 {
		return Result{}, errors.New("VM build log digest mismatch")
	}
	return result, nil
}
