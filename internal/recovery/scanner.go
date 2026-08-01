// Package recovery implements import recovery (SPEC §3.4, §3.5): locating
// candidate video files for a failed import, matching them against the
// expected series and episode via Sonarr's parse endpoint, and auto-importing
// qualifying files through the manual import endpoint.
package recovery

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// maxScanDepth is the maximum directory depth below the scan root that is
// walked (SPEC §3.4 step 2): files up to four path segments below the root.
const maxScanDepth = 4

// videoExtensions are the candidate video file extensions (SPEC §3.4 step 2).
var videoExtensions = map[string]struct{}{
	".mkv": {},
	".mp4": {},
	".avi": {},
	".m4v": {},
	".mov": {},
	".wmv": {},
	".ts":  {},
	".iso": {},
}

// excludedSegment reports whether a directory segment denotes a sample or
// extras directory (SPEC §3.4 step 2): any segment equal to or starting with
// "sample" or "extras", matched case-insensitively.
func excludedSegment(seg string) bool {
	lower := strings.ToLower(seg)
	return strings.HasPrefix(lower, "sample") || strings.HasPrefix(lower, "extras")
}

// Scan walks dir recursively (at most maxScanDepth levels below dir) and
// returns the video files found, sorted deterministically. Directory segments
// named like "sample" or "extras" disqualify their whole subtree; the
// exclusion applies to directory segments only, never to file names. Symbolic
// links are not followed. If dir is itself a video file, it is returned as
// the single candidate.
func Scan(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtree: skip it instead of aborting the whole scan.
			return nil
		}
		if path == dir {
			// dir may itself be a video file (single-file downloads whose
			// output path points at the file, not a folder).
			if !d.IsDir() && d.Type().IsRegular() {
				if _, ok := videoExtensions[strings.ToLower(filepath.Ext(path))]; ok {
					files = append(files, path)
				}
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if depth >= maxScanDepth {
				return filepath.SkipDir
			}
			if excludedSegment(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if depth > maxScanDepth {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := videoExtensions[strings.ToLower(filepath.Ext(path))]; ok {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
