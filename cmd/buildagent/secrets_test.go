package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadPrivateFileRejectsReplaceableSecret(t *testing.T) {
	root := t.TempDir()
	uid := uint32(os.Geteuid())
	secret := filepath.Join(root, "server.key")
	if err := os.WriteFile(secret, []byte("private-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPrivateFile(secret, uid); err != nil || !bytes.Equal(got, []byte("private-key")) {
		t.Fatalf("safe secret rejected: %v", err)
	}
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(secret, uid); err == nil {
		t.Fatal("world-readable secret accepted")
	}
	if err := os.Chmod(secret, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(secret, uid); err == nil {
		t.Fatal("replaceable secret parent accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.key")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(link, uid); err == nil {
		t.Fatal("symlinked secret accepted")
	}
	if _, err := readPrivateFile("relative.key", uid); err == nil {
		t.Fatal("relative secret path accepted")
	}
}
