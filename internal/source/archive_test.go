package source

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectStoredArchiveRequiresCompleteValidZIP(t *testing.T) {
	var content bytes.Buffer
	z := zip.NewWriter(&content)
	f, err := z.Create("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("FROM scratch\n"))
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	id := "12345678-1234-4123-8123-123456789abc"
	want, err := SaveArchive(root, id, bytes.NewReader(content.Bytes()), int64(content.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectStoredArchive(root, id, int64(content.Len()))
	if err != nil || got != want {
		t.Fatalf("inspect: %q %v", got, err)
	}
	if _, err = InspectStoredArchive(root, id, int64(content.Len()+1)); err == nil {
		t.Fatal("wrong length accepted")
	}
	path, _ := ArchivePath(root, id)
	if err = os.WriteFile(path, []byte("not a ZIP"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectStoredArchive(root, id, int64(len("not a ZIP"))); err == nil {
		t.Fatal("invalid ZIP accepted")
	}
}

func TestSaveArchiveIsDurableAndCannotReplaceAnotherArtifact(t *testing.T) {
	root := t.TempDir()
	id := "12345678-1234-4123-8123-123456789abc"
	content := "safe archive content"
	digest, err := SaveArchive(root, id, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(content))
	if digest != "sha256:"+hex.EncodeToString(hash[:]) {
		t.Fatalf("digest: %s", digest)
	}
	path, err := ArchivePath(root, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("saved content: %q %v", data, err)
	}
	if _, err = SaveArchive(root, id, strings.NewReader("overwrite"), 9); err == nil {
		t.Fatal("existing artifact replaced")
	}
	data, _ = os.ReadFile(path)
	if string(data) != content {
		t.Fatal("existing artifact changed")
	}
	if _, err = ArchivePath(root, "../outside"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err = SaveArchive(root, "../outside", strings.NewReader(content), int64(len(content))); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	if _, err = SaveArchive(root, "12345678-1234-4123-8123-123456789abd", strings.NewReader(content), int64(len(content))-1); err == nil {
		t.Fatal("size mismatch accepted")
	}
	link := filepath.Join(t.TempDir(), "source-link")
	if err = os.Symlink(root, link); err == nil {
		if _, err = SaveArchive(link, "12345678-1234-4123-8123-123456789abe", strings.NewReader(content), int64(len(content))); err == nil {
			t.Fatal("symlink root accepted")
		}
	}
}
