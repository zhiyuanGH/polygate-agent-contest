package buildagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type trustedCommandStub struct{ calls []string }

func (s *trustedCommandStub) Run(_ context.Context, _ string, args ...string) (string, error) {
	s.calls = append(s.calls, strings.Join(args, " "))
	switch strings.Join(args, " ") {
	case "buildx ls":
		return "polygate-trusted-slot-0 docker-container\ndefault docker\n", nil
	case "buildx rm --force polygate-trusted-slot-0":
		return "", nil
	case "ps -a --format {{.Names}}":
		return "buildx_buildkit_polygate-trusted-slot-00\nforeign-business", nil
	case "rm -f buildx_buildkit_polygate-trusted-slot-00":
		return "", nil
	case "volume ls --format {{.Name}}":
		return "buildx_buildkit_polygate-trusted-slot-00_state\nforeign-volume", nil
	case "volume rm buildx_buildkit_polygate-trusted-slot-00_state":
		return "", nil
	}
	return "", errors.New("unexpected Docker command")
}

func TestTrustedDockerCleanerRemovesOnlyItsSlot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "slot-0")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "context"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.zip"), []byte("zip"), 0600); err != nil {
		t.Fatal(err)
	}
	stub := &trustedCommandStub{}
	cleaner := DockerCleaner{DockerPath: "/usr/bin/docker", WorkRoot: root, Commands: stub}
	if err := cleaner.StopAndClean(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("slot remains: %v", err)
	}
	for _, call := range stub.calls {
		if strings.Contains(call, "foreign") {
			t.Fatalf("foreign Docker object was mutated: %s", call)
		}
	}
}

func TestTrustedDockerCleanerQuarantinesUnknownSlotFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "slot-0")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unknown"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	cleaner := DockerCleaner{DockerPath: "/usr/bin/docker", WorkRoot: root, Commands: &trustedCommandStub{}}
	if err := cleaner.StopAndClean(context.Background(), 0); err == nil {
		t.Fatal("unknown file did not quarantine the slot")
	}
	if _, err := os.Stat(filepath.Join(dir, "unknown")); err != nil {
		t.Fatalf("unknown file was removed: %v", err)
	}
}
