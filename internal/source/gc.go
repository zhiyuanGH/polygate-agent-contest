package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ArtifactIndex answers whether an archive is still registered in the
// database. A lookup failure must stop cleanup rather than delete the file.
type ArtifactIndex interface {
	SourceArtifactReferenced(context.Context, string) (bool, error)
}

type CleanupStats struct {
	Examined int
	Removed  int
	Bytes    int64
}

var pendingArchiveName = regexp.MustCompile(`^\.pending-[0-9]+\.zip$`)

// PruneOrphanArchives removes only old, unreferenced platform archive files.
// All valid or recently written archives survive even when a concurrent API
// request has not yet committed its metadata.
func PruneOrphanArchives(ctx context.Context, root string, index ArtifactIndex, minAge time.Duration, now time.Time) (CleanupStats, error) {
	var stats CleanupStats
	if !filepath.IsAbs(root) || index == nil || minAge < time.Hour {
		return stats, errors.New("invalid source cleanup configuration")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return stats, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return stats, errors.New("source root must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return stats, err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		id := strings.TrimSuffix(name, ".zip")
		pending := pendingArchiveName.MatchString(name)
		if !pending && (!strings.HasSuffix(name, ".zip") || !archiveID.MatchString(id)) {
			continue
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return stats, err
		}
		age := minAge
		if pending {
			age = 7 * 24 * time.Hour
		}
		if !fileInfo.Mode().IsRegular() || now.Sub(fileInfo.ModTime()) < age {
			continue
		}
		stats.Examined++
		if !pending {
			referenced, err := index.SourceArtifactReferenced(ctx, id)
			if err != nil {
				return stats, fmt.Errorf("source reference lookup: %w", err)
			}
			if referenced {
				continue
			}
		}
		// Verify the entry is still the same file before removal. On Unix this
		// prevents deleting an archive that was replaced since ReadDir.
		current, err := os.Lstat(filepath.Join(root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return stats, err
		}
		if !current.Mode().IsRegular() || !os.SameFile(fileInfo, current) || now.Sub(current.ModTime()) < age {
			continue
		}
		if err = os.Remove(filepath.Join(root, name)); err != nil {
			return stats, err
		}
		stats.Removed++
		stats.Bytes += current.Size()
	}
	return stats, nil
}
