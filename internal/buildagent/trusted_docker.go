package buildagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TrustedDockerRunner is a temporary backend for code supplied by trusted
// participants. It does not provide a security boundary against Dockerfiles.
type TrustedDockerRunner struct {
	Cleaner         DockerCleaner
	HelperPath      string
	HelperSHA256    string
	TrustedOwnerUID uint32
	Commands        CommandRunner
}

func (r TrustedDockerRunner) Validate() error {
	if r.Commands == nil || r.Cleaner.Commands == nil ||
		!filepath.IsAbs(r.Cleaner.WorkRoot) || !filepath.IsAbs(r.HelperPath) ||
		!sha256Pattern.MatchString(r.HelperSHA256) {
		return errors.New("incomplete trusted Docker runner configuration")
	}
	if err := secureOwnerDirectory(r.Cleaner.WorkRoot); err != nil {
		return err
	}
	return securePinnedFile(r.HelperPath, r.HelperSHA256, true, r.TrustedOwnerUID)
}

func (r TrustedDockerRunner) Execute(ctx context.Context, slot int, request Request) (Result, error) {
	return executeHelper(ctx, slot, request, r.Validate, r.Cleaner.WorkRoot,
		r.HelperPath, r.Commands, nil)
}

func (r TrustedDockerRunner) StopAndClean(ctx context.Context, slot int) error {
	return r.Cleaner.StopAndClean(ctx, slot)
}

type DockerCleaner struct {
	DockerPath string
	WorkRoot   string
	Commands   CommandRunner
}

func (c DockerCleaner) StopAndClean(ctx context.Context, slot int) error {
	if slot < 0 || slot >= SlotCount || !filepath.IsAbs(c.WorkRoot) ||
		!filepath.IsAbs(c.DockerPath) || c.Commands == nil {
		return errors.New("invalid trusted Docker cleanup configuration")
	}
	if err := secureOwnerDirectory(c.WorkRoot); err != nil {
		return err
	}
	name := fmt.Sprintf("polygate-trusted-slot-%d", slot)
	container := "buildx_buildkit_" + name + "0"
	volume := container + "_state"
	builders, err := c.Commands.Run(ctx, c.DockerPath, "buildx", "ls")
	if err != nil {
		return err
	}
	if strings.Contains(builders, name) {
		if _, err := c.Commands.Run(ctx, c.DockerPath, "buildx", "rm", "--force", name); err != nil {
			return err
		}
	}
	containers, err := c.Commands.Run(ctx, c.DockerPath, "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	if listed(containers, container) {
		if _, err := c.Commands.Run(ctx, c.DockerPath, "rm", "-f", container); err != nil {
			return err
		}
	}
	volumes, err := c.Commands.Run(ctx, c.DockerPath, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	if listed(volumes, volume) {
		if _, err := c.Commands.Run(ctx, c.DockerPath, "volume", "rm", volume); err != nil {
			return err
		}
	}
	dir := filepath.Join(c.WorkRoot, fmt.Sprintf("slot-%d", slot))
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted build slot is not a real directory")
	}
	if err := rejectSlotMounts(dir); err != nil {
		return err
	}
	for _, name := range []string{"context", "buildkit-state"} {
		path := filepath.Join(dir, name)
		item, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !item.IsDir() || item.Mode()&os.ModeSymlink != 0 {
			return errors.New("trusted build scratch path is not a real directory")
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	for _, name := range []string{"source.zip", "build.log", "result.json", "image.tar", "digest.txt"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// An unexpected file deliberately quarantines the slot.
	for attempt := 0; attempt < 10; attempt++ {
		err = os.Remove(dir)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return err
}

func rejectSlotMounts(dir string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := strings.ReplaceAll(fields[4], `\040`, " ")
		if mount == dir || strings.HasPrefix(mount, dir+string(os.PathSeparator)) {
			return errors.New("trusted build slot contains a mount point")
		}
	}
	return nil
}
