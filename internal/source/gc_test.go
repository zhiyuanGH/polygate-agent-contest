package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type referenceIndex struct {
	refs map[string]bool
	err  error
}

func (r referenceIndex) SourceArtifactReferenced(_ context.Context, id string) (bool, error) {
	return r.refs[id], r.err
}

func TestPruneOrphanArchivesPreservesReferencedAndUncertainFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	keep := "12345678-1234-4123-8123-123456789abc"
	remove := "12345678-1234-4123-8123-123456789abd"
	fresh := "12345678-1234-4123-8123-123456789abe"
	for _, id := range []string{keep, remove, fresh} {
		path := filepath.Join(root, id+".zip")
		if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
			t.Fatal(err)
		}
		when := old
		if id == fresh {
			when = now
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	unknown := filepath.Join(root, "do-not-touch.zip")
	if err := os.WriteFile(unknown, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(root, ".pending-123456.zip")
	if err := os.WriteFile(pending, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	veryOld := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(pending, veryOld, veryOld); err != nil {
		t.Fatal(err)
	}
	stats, err := PruneOrphanArchives(context.Background(), root, referenceIndex{refs: map[string]bool{keep: true}}, 24*time.Hour, now)
	if err != nil || stats.Removed != 2 || stats.Bytes != 14 {
		t.Fatalf("cleanup: %+v %v", stats, err)
	}
	for _, id := range []string{keep, fresh} {
		if _, err = os.Stat(filepath.Join(root, id+".zip")); err != nil {
			t.Fatalf("preserved %s: %v", id, err)
		}
	}
	if _, err = os.Stat(filepath.Join(root, remove+".zip")); !os.IsNotExist(err) {
		t.Fatalf("orphan still present: %v", err)
	}
	if _, err = os.Stat(unknown); err != nil {
		t.Fatal(err)
	}
	// A failed DB lookup must never turn into permission to delete.
	_, err = PruneOrphanArchives(context.Background(), root, referenceIndex{err: errors.New("database down")}, 24*time.Hour, now)
	if err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("lookup error: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, keep+".zip")); err != nil {
		t.Fatal(err)
	}
}
