package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type buildLogIndexForTest struct {
	refs map[string]bool
	err  error
}

func (i buildLogIndexForTest) SourceBuildReferenced(_ context.Context, id string) (bool, error) {
	return i.refs[id], i.err
}

func TestBuildLogCleanupPreservesReferencedAndRecentOutput(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	for j, id := range ids {
		path, err := BuildLogPath(root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("log!"), 0600); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-48 * time.Hour)
		if j == 2 {
			when = now
		}
		if err = os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := PruneOrphanBuildLogs(context.Background(), root,
		buildLogIndexForTest{refs: map[string]bool{ids[0]: true}}, 24*time.Hour, now)
	if err != nil || stats.Removed != 1 || stats.Bytes != 4 {
		t.Fatalf("orphan cleanup: %+v %v", stats, err)
	}
	for _, id := range []string{ids[0], ids[2]} {
		path, _ := BuildLogPath(root, id)
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("preserved %s: %v", id, err)
		}
	}
	_, err = PruneOrphanBuildLogs(context.Background(), root,
		buildLogIndexForTest{err: errors.New("database down")}, 24*time.Hour, now)
	if err == nil {
		t.Fatal("database failure allowed log deletion")
	}
	path, _ := BuildLogPath(root, ids[0])
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLogBudgetPrunesOldestWithoutTouchingActiveTask(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	for j, id := range ids {
		path, _ := BuildLogPath(root, id)
		if err := os.WriteFile(path, []byte("1234"), 0600); err != nil {
			t.Fatal(err)
		}
		when := now.Add(time.Duration(j-3) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := PruneBuildLogsForBudget(context.Background(), root, 8, 30*time.Minute, now, ids[0])
	if err != nil || stats.Removed != 1 || stats.Bytes != 4 {
		t.Fatalf("budget cleanup: %+v %v", stats, err)
	}
	if _, err = os.Stat(filepath.Join(root, ids[0]+".log")); err != nil {
		t.Fatal("active task log removed")
	}
	if _, err = os.Stat(filepath.Join(root, ids[1]+".log")); !os.IsNotExist(err) {
		t.Fatalf("oldest eligible log survived: %v", err)
	}
	_, err = PruneBuildLogsForBudget(context.Background(), root, 1, 30*time.Minute, now, ids[0])
	if !errors.Is(err, ErrBuildLogBudget) {
		t.Fatalf("expected protected budget exhaustion, got %v", err)
	}
}
