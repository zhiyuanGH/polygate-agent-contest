package buildagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeVirsh struct {
	active string
	all    string
	fail   bool
	calls  []string
}

func (f *fakeVirsh) Run(_ context.Context, _ string, args ...string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.fail {
		return "", errors.New("libvirt unavailable")
	}
	if strings.Contains(line, "list --all --name") {
		return f.all, nil
	}
	if strings.Contains(line, "list --name") {
		return f.active, nil
	}
	return "", nil
}

func TestLibvirtCleanerDestroysFixedGuestBeforeDeletingFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "slot-0")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"overlay.qcow2", "buildkit.qcow2", "seed.iso", "key", "source.zip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	virsh := &fakeVirsh{active: "polygate-build-slot-0\nother-vm\n", all: "polygate-build-slot-0\nother-vm\n"}
	cleaner := LibvirtCleaner{VirshPath: "/usr/bin/virsh", WorkRoot: root, Commands: virsh}
	if err := cleaner.StopAndClean(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("slot files remain: %v", err)
	}
	joined := strings.Join(virsh.calls, "\n")
	if !strings.Contains(joined, "destroy polygate-build-slot-0") || !strings.Contains(joined, "undefine polygate-build-slot-0") ||
		strings.Contains(joined, "destroy other-vm") {
		t.Fatalf("unexpected guest operations: %s", joined)
	}
}

func TestLibvirtCleanerQuarantinesOnUnknownFileOrHypervisorFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "slot-1")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "unexpected")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	virsh := &fakeVirsh{fail: true}
	cleaner := LibvirtCleaner{VirshPath: "/usr/bin/virsh", WorkRoot: root, Commands: virsh}
	if err := cleaner.StopAndClean(context.Background(), 1); err == nil {
		t.Fatal("hypervisor failure ignored")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("files removed before VM stop", err)
	}
	virsh.fail = false
	if err := cleaner.StopAndClean(context.Background(), 1); err == nil {
		t.Fatal("unknown file silently removed")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unknown file removed", err)
	}
}
