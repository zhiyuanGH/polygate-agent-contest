package source

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadChunksAreImmutableAndAssembled(t *testing.T) {
	root := t.TempDir()
	id := "12345678-1234-4123-8123-123456789abc"
	item := Upload{ID: id, Owner: "student-a", Size: UploadChunkBytes + 3, Template: "dockerfile"}
	if err := CreateUpload(root, item); err != nil {
		t.Fatal(err)
	}
	loaded, dir, err := LoadUpload(root, id, "student-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = LoadUpload(root, id, "student-b"); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("cross-account load: %v", err)
	}
	if loaded.ChunkCount() != 2 || loaded.ChunkSize(0) != UploadChunkBytes || loaded.ChunkSize(1) != 3 {
		t.Fatal("incorrect chunk boundaries")
	}
	first := bytes.Repeat([]byte("a"), int(UploadChunkBytes))
	if err = SaveUploadChunk(dir, loaded, 0, bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	if err = SaveUploadChunk(dir, loaded, 0, bytes.NewReader(first)); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if err = SaveUploadChunk(dir, loaded, 0, bytes.NewReader(bytes.Repeat([]byte("b"), int(UploadChunkBytes)))); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("changed retry: %v", err)
	}
	if _, err = AssembleUpload(dir, loaded); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("missing chunk: %v", err)
	}
	if err = SaveUploadChunk(dir, loaded, 1, bytes.NewReader([]byte("xyz"))); err != nil {
		t.Fatal(err)
	}
	f, err := AssembleUpload(dir, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if stat, err := f.Stat(); err != nil || stat.Size() != loaded.Size {
		t.Fatalf("assembled size: %v %v", stat, err)
	}
	if _, err = f.Seek(UploadChunkBytes, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	end, err := io.ReadAll(f)
	if err != nil || string(end) != "xyz" {
		t.Fatalf("assembled tail: %q %v", end, err)
	}
	if info, err := os.Lstat(filepath.Join(dir, "00.part")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("first chunk changed: %v", err)
	}
}

func TestUploadStagingQuotaAndExpiry(t *testing.T) {
	root := t.TempDir()
	ids := []string{
		"12345678-1234-4123-8123-123456789abc",
		"22345678-1234-4123-8123-123456789abc",
		"32345678-1234-4123-8123-123456789abc",
		"42345678-1234-4123-8123-123456789abc",
	}
	for _, id := range ids[:3] {
		if err := CreateUpload(root, Upload{ID: id, Owner: "student-a", Size: 1, Template: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := CreateUpload(root, Upload{ID: ids[3], Owner: "student-a", Size: 1, Template: "dockerfile"}); !errors.Is(err, ErrUploadQuota) {
		t.Fatalf("quota: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	dir := filepath.Join(root, ".uploads", ids[0])
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	n, err := PruneUploads(root, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := CreateUpload(root, Upload{ID: ids[3], Owner: "student-a", Size: 1, Template: "dockerfile"}); err != nil {
		t.Fatalf("quota after cleanup: %v", err)
	}
}
