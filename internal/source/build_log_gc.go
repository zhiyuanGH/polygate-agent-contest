package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const MaxRetainedBuildLogBytes int64 = 8 << 30

var ErrBuildLogBudget = errors.New("build log storage budget is exhausted")

type BuildLogIndex interface {
	SourceBuildReferenced(context.Context, string) (bool, error)
}

type buildLogEntry struct {
	path string
	info os.FileInfo
}

func scanBuildLogs(root string) ([]buildLogEntry, int64, error) {
	if !filepath.IsAbs(root) {
		return nil, 0, errors.New("build log root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errors.New("build log root must be a real directory")
	}
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, 0, err
	}
	logs := make([]buildLogEntry, 0)
	var total int64
	for _, item := range items {
		if !item.Type().IsRegular() || !strings.HasSuffix(item.Name(), ".log") ||
			!archiveID.MatchString(strings.TrimSuffix(item.Name(), ".log")) {
			continue
		}
		fileInfo, err := item.Info()
		if err != nil {
			return nil, 0, err
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Size() < 0 || fileInfo.Size() > 64<<20 || total > int64(^uint64(0)>>1)-fileInfo.Size() {
			return nil, 0, errors.New("invalid build log file")
		}
		total += fileInfo.Size()
		logs = append(logs, buildLogEntry{path: filepath.Join(root, item.Name()), info: fileInfo})
	}
	return logs, total, nil
}

func removeStableBuildLog(item buildLogEntry, minAge time.Duration, now time.Time) (bool, error) {
	current, err := os.Lstat(item.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(item.info, current) ||
		current.Size() != item.info.Size() || !current.ModTime().Equal(item.info.ModTime()) ||
		now.Sub(current.ModTime()) < minAge {
		return false, nil
	}
	if err = os.Remove(item.path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return err == nil, nil
}

// PruneOrphanBuildLogs closes the crash window between database retention
// deletion and file deletion. Uncertain database lookups fail closed.
func PruneOrphanBuildLogs(ctx context.Context, root string, index BuildLogIndex, minAge time.Duration, now time.Time) (CleanupStats, error) {
	var stats CleanupStats
	if index == nil || minAge < time.Hour {
		return stats, errors.New("invalid build log cleanup configuration")
	}
	logs, _, err := scanBuildLogs(root)
	if err != nil {
		return stats, err
	}
	for _, item := range logs {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		if now.Sub(item.info.ModTime()) < minAge {
			continue
		}
		stats.Examined++
		id := strings.TrimSuffix(filepath.Base(item.path), ".log")
		referenced, err := index.SourceBuildReferenced(ctx, id)
		if err != nil {
			return stats, fmt.Errorf("build log reference lookup: %w", err)
		}
		if referenced {
			continue
		}
		removed, err := removeStableBuildLog(item, minAge, now)
		if err != nil {
			return stats, err
		}
		if removed {
			stats.Removed++
			stats.Bytes += item.info.Size()
		}
	}
	return stats, nil
}

// PruneBuildLogsForBudget keeps at most budget bytes of build output. Only
// inactive logs may be reclaimed; a current VM build lasts at most 15 minutes.
func PruneBuildLogsForBudget(ctx context.Context, root string, budget int64, minAge time.Duration, now time.Time, protectedTaskID string) (CleanupStats, error) {
	var stats CleanupStats
	if budget < 1 || minAge < 20*time.Minute {
		return stats, errors.New("invalid build log budget")
	}
	logs, total, err := scanBuildLogs(root)
	if err != nil {
		return stats, err
	}
	stats.Examined = len(logs)
	if total <= budget {
		return stats, nil
	}
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].info.ModTime().Equal(logs[j].info.ModTime()) {
			return logs[i].path < logs[j].path
		}
		return logs[i].info.ModTime().Before(logs[j].info.ModTime())
	})
	for _, item := range logs {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		if total <= budget {
			break
		}
		if protectedTaskID != "" && filepath.Base(item.path) == protectedTaskID+".log" {
			continue
		}
		removed, err := removeStableBuildLog(item, minAge, now)
		if err != nil {
			return stats, err
		}
		if removed {
			total -= item.info.Size()
			stats.Removed++
			stats.Bytes += item.info.Size()
		}
	}
	if total > budget {
		return stats, ErrBuildLogBudget
	}
	return stats, nil
}
