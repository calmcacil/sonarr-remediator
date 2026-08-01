package recovery

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeFile creates path (and any parent directories) with a dummy payload.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestScanCollectsVideoFilesAndSkipsExcluded builds a temp tree with video
// files at depths 1..4 (both cases), non-video files, sample/extras subtrees,
// and a depth-5 file, and asserts the exact collected set in sorted order.
func TestScanCollectsVideoFilesAndSkipsExcluded(t *testing.T) {
	root := t.TempDir()

	// Every file placed in the tree. Files marked collected are video files
	// within depth 4 that are not under a sample/extras directory segment.
	all := []string{
		// depth 0
		"alpha.mkv",  // collected
		"Bravo.MP4",  // collected (uppercase extension)
		"sample.mkv", // collected: exclusion applies to dirs only, never file names
		"readme.nfo", // not video
		// depth 1
		"Season 1/charlie.avi", // collected
		"Season 1/delta.m4v",   // collected
		"Season 1/notes.txt",   // not video
		// depth 2
		"Season 1/Episode 2/echo.mov",    // collected
		"Season 1/Episode 2/foxtrot.wmv", // collected
		"Season 1/Episode 2/art.jpg",     // not video
		// depth 3
		"Season 1/Episode 2/Subtitles/golf.ts",     // collected
		"Season 1/Episode 2/Subtitles/hotel.iso",   // collected
		"Season 1/Episode 2/Subtitles/subs.par2",   // not video
		"Season 1/Episode 2/Subtitles/sample/x.ts", // excluded: sample segment at depth 3
		// depth 4
		"Season 1/Episode 2/Subtitles/deeper/india.mkv",  // collected
		"Season 1/Episode 2/Subtitles/deeper/juliet.avi", // collected
		// depth 5 (parent dir "lost" at depth 4 is skipped)
		"Season 1/Episode 2/Subtitles/deeper/lost/kilo.mp4", // not collected
		// sample/extras subtrees at any depth are excluded entirely
		"sample/Sample.episode.mkv",
		"Sample/Cap.Episode.avi",
		"extras/Extra.Scene.mp4",
		"Season 1/Episode 2/Extras/Extra.episode.mov",
		"Season 1/Episode 2/Subtitles/SAMPLE/Sample.episode.ts",
	}
	for _, f := range all {
		writeFile(t, filepath.Join(root, f))
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	wantRel := []string{
		"alpha.mkv",
		"Bravo.MP4",
		"sample.mkv",
		"Season 1/charlie.avi",
		"Season 1/delta.m4v",
		"Season 1/Episode 2/Subtitles/deeper/india.mkv",
		"Season 1/Episode 2/Subtitles/deeper/juliet.avi",
		"Season 1/Episode 2/Subtitles/golf.ts",
		"Season 1/Episode 2/Subtitles/hotel.iso",
		"Season 1/Episode 2/echo.mov",
		"Season 1/Episode 2/foxtrot.wmv",
	}
	want := make([]string, 0, len(wantRel))
	for _, r := range wantRel {
		want = append(want, filepath.Join(root, r))
	}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("Scan returned %d files, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatch at index %d: got %q, want %q\n got: %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("result is not sorted: %v", got)
	}
}

// TestScanEmptyDir asserts an empty directory yields an empty result.
func TestScanEmptyDir(t *testing.T) {
	got, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %v, want empty", got)
	}
}

// TestScanMissingDir documents the real behavior for a non-existent root:
// the walk function swallows the root Lstat error (see scanner.go's walk
// callback, which skips unreadable subtrees instead of aborting), so Scan
// returns an empty result with no error.
func TestScanMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := Scan(missing)
	if err != nil {
		t.Fatalf("Scan(%s) returned error %v; real behavior is an empty result, nil error", missing, err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan(%s) returned %v, want empty", missing, got)
	}
}

// TestScanVideoFileRoot asserts that a single video file passed as the scan
// root is returned as the lone candidate.
func TestScanVideoFileRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	writeFile(t, file)

	got, err := Scan(file)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0] != file {
		t.Fatalf("Scan(%s) = %v, want [%s]", file, got, file)
	}
}

// TestScanNonVideoFileRoot asserts that a non-video file passed as the scan
// root yields no candidates.
func TestScanNonVideoFileRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	writeFile(t, file)

	got, err := Scan(file)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan(%s) = %v, want empty", file, got)
	}
}

// TestScanDoesNotFollowSymlinks asserts symbolic links are not collected.
func TestScanDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real.mkv")
	writeFile(t, real)
	if err := os.Symlink(real, filepath.Join(root, "link.mkv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0] != real {
		t.Fatalf("Scan(%s) = %v, want [%s]", root, got, real)
	}
}
