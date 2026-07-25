package recovery

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// newTestScanner is a helper.
func newTestScanner() *Scanner {
	return NewScanner()
}

func TestFindVideoFiles_FindsVideoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .mkv and .mp4 files
	mkvPath := filepath.Join(tmpDir, "episode.mkv")
	mp4Path := filepath.Join(tmpDir, "episode.mp4")
	os.WriteFile(mkvPath, []byte("test"), 0644)
	os.WriteFile(mp4Path, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 video files, got %d: %v", len(files), files)
	}

	// Verify both files are found
	found := make(map[string]bool)
	for _, f := range files {
		found[f] = true
	}
	if !found[mkvPath] {
		t.Error("expected to find episode.mkv")
	}
	if !found[mp4Path] {
		t.Error("expected to find episode.mp4")
	}
}

func TestFindVideoFiles_ExcludesSamples(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a "sample" directory with a video file inside
	sampleDir := filepath.Join(tmpDir, "sample")
	os.MkdirAll(sampleDir, 0755)
	sampleFile := filepath.Join(sampleDir, "sample.mkv")
	os.WriteFile(sampleFile, []byte("test"), 0644)

	// Also create a normal episode.mkv at the root
	episodePath := filepath.Join(tmpDir, "episode.mkv")
	os.WriteFile(episodePath, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 video file (sample dir excluded), got %d: %v", len(files), files)
	}
	if files[0] != episodePath {
		t.Errorf("expected only episode.mkv, got %s", files[0])
	}
}

func TestFindVideoFiles_ExcludesExtras(t *testing.T) {
	tmpDir := t.TempDir()

	// Create "extras" directory with a video file
	extrasDir := filepath.Join(tmpDir, "extras")
	os.MkdirAll(extrasDir, 0755)
	extrasFile := filepath.Join(extrasDir, "featurette.mkv")
	os.WriteFile(extrasFile, []byte("test"), 0644)

	// Normal episode at root
	episodePath := filepath.Join(tmpDir, "episode.mkv")
	os.WriteFile(episodePath, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 video file (extras dir excluded), got %d: %v", len(files), files)
	}
	if files[0] != episodePath {
		t.Errorf("expected only episode.mkv, got %s", files[0])
	}
}

func TestFindVideoFiles_ExcludesNonVideo(t *testing.T) {
	tmpDir := t.TempDir()

	// Create various non-video files
	os.WriteFile(filepath.Join(tmpDir, "info.nfo"), []byte("info"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "readme.txt"), []byte("readme"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "subs.srt"), []byte("subs"), 0644)

	// One legitimate video file
	episodePath := filepath.Join(tmpDir, "episode.mkv")
	os.WriteFile(episodePath, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 video file, got %d", len(files))
	}
	if files[0] != episodePath {
		t.Errorf("expected episode.mkv, got %s", files[0])
	}
}

func TestFindVideoFiles_MaxDepth(t *testing.T) {
	tmpDir := t.TempDir()

	// Create nested directories that exceed maxDepth (4)
	deepDir := filepath.Join(tmpDir, "l1", "l2", "l3", "l4")
	os.MkdirAll(deepDir, 0755)
	tooDeep := filepath.Join(deepDir, "too_deep.mkv")
	os.WriteFile(tooDeep, []byte("test"), 0644)

	// Also create a file at the root
	episodePath := filepath.Join(tmpDir, "episode.mkv")
	os.WriteFile(episodePath, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	// Should only find episode.mkv, not too_deep.mkv (depth=5 > 4)
	if len(files) != 1 {
		t.Fatalf("expected 1 video file (depth exceeded), got %d: %v", len(files), files)
	}
	if files[0] != episodePath {
		t.Errorf("expected episode.mkv, got %s", files[0])
	}
}

func TestFindVideoFiles_HiddenDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a hidden directory with a video file
	hiddenDir := filepath.Join(tmpDir, ".hidden")
	os.MkdirAll(hiddenDir, 0755)
	hiddenFile := filepath.Join(hiddenDir, "secret.mkv")
	os.WriteFile(hiddenFile, []byte("test"), 0644)

	// Normal episode at root
	episodePath := filepath.Join(tmpDir, "episode.mkv")
	os.WriteFile(episodePath, []byte("test"), 0644)

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 video file (hidden dir skipped), got %d: %v", len(files), files)
	}
	if files[0] != episodePath {
		t.Errorf("expected episode.mkv, got %s", files[0])
	}
}

func TestFindVideoFiles_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("expected empty result for empty directory, got %d files", len(files))
	}
}

func TestFindVideoFiles_MixedFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a mix of files
	allFiles := []struct {
		name     string
		expected bool // should be found?
	}{
		{"show_s01e01.mkv", true},
		{"show_s01e01.mp4", true},
		{"show_s01e01.avi", true},
		{"show_s01e01.m4v", true},
		{"show_s01e01.mov", true},
		{"subs.srt", false},
		{"info.nfo", false},
		{"cover.jpg", false},
		{"readme.txt", false},
		{"checksum.md5", false},
	}

	for _, f := range allFiles {
		os.WriteFile(filepath.Join(tmpDir, f.name), []byte("test"), 0644)
	}

	scanner := newTestScanner()
	files, err := scanner.FindVideoFiles(tmpDir)
	if err != nil {
		t.Fatalf("FindVideoFiles: unexpected error: %v", err)
	}

	// Sort results for consistent comparison
	sort.Strings(files)

	expectedCount := 0
	for _, f := range allFiles {
		if f.expected {
			expectedCount++
		}
	}

	if len(files) != expectedCount {
		t.Errorf("expected %d video files, got %d", expectedCount, len(files))
	}

	for _, f := range allFiles {
		p := filepath.Join(tmpDir, f.name)
		_, found := findInSlice(files, p)
		if f.expected && !found {
			t.Errorf("expected to find %q but it was not returned", f.name)
		}
		if !f.expected && found {
			t.Errorf("expected to NOT find %q but it was returned", f.name)
		}
	}
}

// findInSlice checks if a string is present in a slice.
func findInSlice(slice []string, target string) (int, bool) {
	for i, s := range slice {
		if s == target {
			return i, true
		}
	}
	return -1, false
}
