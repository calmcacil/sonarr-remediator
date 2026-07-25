package executor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
)

// CleanupConfig holds settings for cleanup operations.
type CleanupConfig struct {
	RemoveEmptyFolders   bool
	RemoveSampleFiles    bool
	RemoveNFOFiles       bool
	RemoveBrokenSymlinks bool
	RemoveTempExtraction bool
	RemovePartialUnpack  bool
	ExcludePatterns      []string
	SampleMaxSizeMB      int64
	NFOMaxSizeMB         int64
	TempAgeHours         time.Duration
	PartialUnpackAgeHours time.Duration
	SamplePatterns       []string
	TempPatterns         []string
}

// RunCleanup performs configured cleanup operations on download roots.
func RunCleanup(ctx context.Context, roots []string, cfg CleanupConfig, dryRun bool) {
	for _, root := range roots {
		if cfg.RemoveEmptyFolders {
			removeEmptyFolders(root, cfg.ExcludePatterns, dryRun)
		}
		if cfg.RemoveBrokenSymlinks {
			removeBrokenSymlinks(root, dryRun)
		}
		if cfg.RemoveNFOFiles {
			removeNFOFiles(root, cfg.NFOMaxSizeMB, dryRun)
		}
		if cfg.RemoveSampleFiles {
			removeSampleFiles(root, cfg.SamplePatterns, cfg.SampleMaxSizeMB, dryRun)
		}
		if cfg.RemoveTempExtraction {
			removeTempExtraction(root, cfg.TempPatterns, cfg.TempAgeHours, dryRun)
		}
		if cfg.RemovePartialUnpack {
			removePartialUnpackFiles(root, cfg.PartialUnpackAgeHours, dryRun)
		}
	}
}

func removeEmptyFolders(root string, excludePatterns []string, dryRun bool) {
	// Walk bottom-up to find empty dirs
	var dirs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})

	// Sort by depth descending (deepest first)
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(filepath.ToSlash(dirs[i]), "/") > strings.Count(filepath.ToSlash(dirs[j]), "/")
	})

	for _, dir := range dirs {
		if isEmptyDir(dir, excludePatterns) {
			if dryRun {
				logging.Logger.Info("DRY RUN: would remove empty dir", "path", dir)
			} else {
				if err := os.Remove(dir); err == nil {
					logging.Logger.Info("removed empty dir", "path", dir)
				}
			}
		}
	}
}

func isEmptyDir(dir string, excludePatterns []string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	if len(entries) > 0 {
		return false
	}
	// Check exclude patterns against dir name
	for _, pat := range excludePatterns {
		matched, _ := filepath.Match(pat, filepath.Base(dir))
		if matched {
			return false
		}
	}
	return true
}

func removeBrokenSymlinks(root string, dryRun bool) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				if dryRun {
					logging.Logger.Info("DRY RUN: would remove broken symlink", "path", path)
				} else {
					os.Remove(path)
				}
			}
		}
		return nil
	})
}

func removeNFOFiles(root string, maxSizeMB int64, dryRun bool) {
	maxSize := maxSizeMB * 1024 * 1024
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(d.Name()) == ".nfo" {
			info, err := d.Info()
			if err == nil && info.Size() < maxSize {
				if dryRun {
					logging.Logger.Info("DRY RUN: would remove nfo", "path", path)
				} else {
					os.Remove(path)
				}
			}
		}
		return nil
	})
}

func removeSampleFiles(root string, patterns []string, maxSizeMB int64, dryRun bool) {
	maxSize := maxSizeMB * 1024 * 1024
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, match := range matches {
			info, err := os.Stat(match)
			if err == nil && info.Size() < maxSize {
				if dryRun {
					logging.Logger.Info("DRY RUN: would remove sample", "path", match)
				} else {
					os.Remove(match)
				}
			}
		}
	}
}

func removeTempExtraction(root string, patterns []string, ageHours time.Duration, dryRun bool) {
	cutoff := time.Now().Add(-ageHours)
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, match := range matches {
			info, err := os.Stat(match)
			if err == nil && info.ModTime().Before(cutoff) {
				if dryRun {
					logging.Logger.Info("DRY RUN: would remove temp dir", "path", match)
				} else {
					os.RemoveAll(match)
				}
			}
		}
	}
}

func removePartialUnpackFiles(root string, ageHours time.Duration, dryRun bool) {
	cutoff := time.Now().Add(-ageHours)
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext == ".part" || ext == ".partial" {
			info, err := d.Info()
			if err == nil && info.ModTime().Before(cutoff) {
				if dryRun {
					logging.Logger.Info("DRY RUN: would remove partial file", "path", path)
				} else {
					os.Remove(path)
				}
			}
		}
		return nil
	})
}
