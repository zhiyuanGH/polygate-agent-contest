package buildagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostLockRejectsConcurrentAgentAndSymlink(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireHostLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireHostLock(root); err == nil {
		second.Close()
		t.Fatal("second agent acquired the same host lock")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireHostLock(root)
	if err != nil {
		t.Fatalf("lock was not released after close: %v", err)
	}
	third.Close()
	if err = os.Remove(filepath.Join(root, "agent.lock")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "unrelated"), filepath.Join(root, "agent.lock")); err != nil {
		t.Fatal(err)
	}
	if file, err := AcquireHostLock(root); err == nil {
		file.Close()
		t.Fatal("symlink lock was accepted")
	}
}
