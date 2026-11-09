package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"polygate/internal/domain"
	"polygate/internal/postgres"
	"polygate/internal/server"
	"polygate/internal/source"
)

func main() {
	root := os.Getenv("POLYGATE_SOURCE_ROOT")
	info, statErr := os.Lstat(root)
	if !filepath.IsAbs(root) || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		log.Fatal("POLYGATE_SOURCE_ROOT must be an existing real absolute directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := postgres.Open(ctx, postgres.DatabaseURLFromEnvironment())
	if err != nil {
		log.Fatal("database connection failed")
	}
	defer store.Close()
	revoked, err := store.ReconcileRevokedSourceBuilds(ctx)
	if err != nil {
		log.Fatal("source grant revocation cleanup failed: ", err)
	}
	preparing, err := store.ListPreparingSourceBuilds(ctx, 5*time.Minute)
	if err != nil {
		log.Fatal("source recovery query failed: ", err)
	}
	recovered := 0
	for _, task := range preparing {
		digest, inspectErr := source.InspectStoredArchive(root, task.ArtifactID, task.SizeBytes)
		if os.IsNotExist(inspectErr) {
			continue
		}
		if inspectErr != nil {
			if _, err = store.AbandonSourceBuild(ctx, task.AccountID, task.ArtifactID); err != nil {
				log.Fatal("source recovery failure update: ", err)
			}
			continue
		}
		_, err = store.ActivateSourceBuild(ctx, task.AccountID, task.ArtifactID, digest)
		if errors.Is(err, server.ErrNotFound) {
			continue
		}
		var domainErr *domain.Error
		if errors.Is(err, server.ErrDisabled) ||
			(errors.As(err, &domainErr) && domainErr.Code == "BUILD_NOT_ENABLED") {
			// Revocation prevents activation; ordinary retention expires the
			// Preparing reservation and archive after 24 hours.
			continue
		}
		if err != nil {
			log.Fatal("source recovery activation failed: ", err)
		}
		recovered++
	}
	ids, err := store.ExpireSourceBuilds(ctx)
	if err != nil {
		log.Fatal("source retention cleanup failed: ", err)
	}
	for _, item := range ids {
		path, pathErr := source.ArchivePath(root, item.ArtifactID)
		if pathErr != nil {
			log.Fatal("invalid stored archive ID")
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Fatal("archive removal failed: ", err)
		}
		logPath, pathErr := source.BuildLogPath(root, item.TaskID)
		if pathErr != nil {
			log.Fatal("invalid stored build log ID")
		}
		if err = os.Remove(logPath); err != nil && !os.IsNotExist(err) {
			log.Fatal("build log removal failed: ", err)
		}
	}
	stats, err := source.PruneOrphanArchives(ctx, root, store, 24*time.Hour, time.Now())
	if err != nil {
		log.Fatal("orphan cleanup failed: ", err)
	}
	logStats, err := source.PruneOrphanBuildLogs(ctx, root, store, 24*time.Hour, time.Now())
	if err != nil {
		log.Fatal("orphan build log cleanup failed: ", err)
	}
	budgetStats, err := source.PruneBuildLogsForBudget(ctx, root, source.MaxRetainedBuildLogBytes,
		30*time.Minute, time.Now(), "")
	if err != nil {
		log.Fatal("build log budget cleanup failed: ", err)
	}
	uploadsRemoved, err := source.PruneUploads(root, time.Now())
	if err != nil {
		log.Fatal("upload staging cleanup failed: ", err)
	}
	log.Printf("source cleanup: revoked=%d recovered=%d expired=%d orphan_examined=%d orphan_removed=%d orphan_bytes=%d log_orphan_removed=%d log_budget_removed=%d upload_staging_removed=%d",
		revoked, recovered, len(ids), stats.Examined, stats.Removed, stats.Bytes, logStats.Removed, budgetStats.Removed, uploadsRemoved)
}
