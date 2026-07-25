package recovery

import (
	"os"
	"path/filepath"
	"strings"
)

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".wmv": true, ".ts": true, ".iso": true,
}

var excludeNames = map[string]bool{
	"sample": true, "extras": true,
}

var excludeExtensions = map[string]bool{
	".nfo": true, ".txt": true, ".jpg": true, ".jpeg": true,
	".png": true, ".sfv": true, ".par2": true, ".srt": true,
	".sub": true, ".idx": true, ".md5": true, ".exe": true,
}

// maxDepth is the maximum directory depth from download root.
const maxDepth = 4

// Scanner finds candidate video files.
type Scanner struct{}

// NewScanner creates a file scanner.
func NewScanner() *Scanner {
	return &Scanner{}
}

// FindVideoFiles scans a directory for candidate video files.
func (s *Scanner) FindVideoFiles(downloadRoot string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(downloadRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible
		}

		// Exclude hidden files/dirs
		if strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Check depth
		rel, err := filepath.Rel(downloadRoot, path)
		if err != nil {
			return nil
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			lower := strings.ToLower(d.Name())
			if excludeNames[lower] || strings.HasPrefix(lower, "sample") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if excludeExtensions[ext] {
			return nil
		}
		if !videoExtensions[ext] {
			return nil
		}

		files = append(files, path)
		return nil
	})

	return files, err
}
