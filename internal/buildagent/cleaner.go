package buildagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CommandRunner is injectable so recovery ordering can be tested without a
// hypervisor. The production implementation never invokes a shell.
type CommandRunner interface {
	Run(context.Context, string, ...string) (string, error)
}

type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, program string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var output limitedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%s failed: %w: %s", filepath.Base(program), err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data) < 4096 {
		keep := 4096 - len(b.data)
		if len(p) < keep {
			keep = len(p)
		}
		b.data = append(b.data, p[:keep]...)
	}
	return n, nil
}

func (b *limitedOutput) String() string { return string(b.data) }

type LibvirtCleaner struct {
	VirshPath string
	WorkRoot  string
	Commands  CommandRunner
}

var slotFiles = [...]string{
	"overlay.qcow2", "buildkit.qcow2", "seed.iso", "key", "key.pub",
	"user-data", "meta-data", "network-config", "source.zip", "build.log",
	"result.json", "image.tar", "known_hosts", "digest.txt",
}

func (c LibvirtCleaner) StopAndClean(ctx context.Context, slot int) error {
	if slot < 0 || slot >= SlotCount || !filepath.IsAbs(c.WorkRoot) || !filepath.IsAbs(c.VirshPath) || c.Commands == nil {
		return errors.New("invalid libvirt cleanup configuration")
	}
	root, err := os.Lstat(c.WorkRoot)
	if err != nil {
		return err
	}
	if !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return errors.New("build work root is not a real directory")
	}
	name := fmt.Sprintf("polygate-build-slot-%d", slot)
	active, err := c.Commands.Run(ctx, c.VirshPath, "-c", "qemu:///system", "list", "--name")
	if err != nil {
		return err
	}
	if listed(active, name) {
		if _, err = c.Commands.Run(ctx, c.VirshPath, "-c", "qemu:///system", "destroy", name); err != nil {
			return err
		}
	}
	all, err := c.Commands.Run(ctx, c.VirshPath, "-c", "qemu:///system", "list", "--all", "--name")
	if err != nil {
		return err
	}
	if listed(all, name) {
		if _, err = c.Commands.Run(ctx, c.VirshPath, "-c", "qemu:///system", "undefine", name); err != nil {
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
		return errors.New("build slot directory is not a real directory")
	}
	for _, file := range slotFiles {
		err = os.Remove(filepath.Join(dir, file))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// Unknown files are intentionally left in place. That causes cleanup to
	// fail and the manager to quarantine the slot for operator inspection.
	return os.Remove(dir)
}

func listed(output, name string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}
