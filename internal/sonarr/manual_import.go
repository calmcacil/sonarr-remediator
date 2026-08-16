package sonarr

import (
	"context"
	"net/http"
	"net/url"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ManualImportPreview lists the importable files of a queue item's download
// folder, asking Sonarr to perform its own parsing and series/episode
// matching (GET /api/v3/manualimport). It mirrors the UI call exactly:
// downloadId only — Sonarr derives the folder from the tracked download and
// anchors the match to the grab history (SPEC §12) — plus
// filterExistingFiles=false, since every reconciled episode already has a
// media file and the default filter would drop the candidate. Returns the
// previewed files with Sonarr's quality, languages, and matched episodes;
// files Sonarr could not match carry rejections.
func (c *Client) ManualImportPreview(ctx context.Context, downloadID string) ([]types.ManualImportFile, error) {
	q := url.Values{}
	q.Set("downloadId", downloadID)
	q.Set("filterExistingFiles", "false")
	var res []types.ManualImportFile
	if err := c.do(ctx, http.MethodGet, "/api/v3/manualimport", q, nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// ManualImportCommand triggers the ManualImport command
// (POST /api/v3/command), the actual import step of Sonarr's manual-import
// flow (SPEC §12). The command imports the submitted files unconditionally —
// the upgrade and custom-format gates of the automatic import path do not
// apply, exactly like the UI's manual Import button — and completes the
// tracked download, removing the queue item once the import finishes.
func (c *Client) ManualImportCommand(ctx context.Context, cmd types.ManualImportCommand) error {
	return c.do(ctx, http.MethodPost, "/api/v3/command", nil, cmd, nil)
}

// EpisodeSearch triggers an EpisodeSearch command (POST /api/v3/command) for
// the given episodes — the same command Sonarr itself issues when a download
// fails, searching for a replacement release regardless of the missing-file
// state (SPEC §3.9). Used as the redownload fallback when no grabbed history
// item could be blocklisted.
func (c *Client) EpisodeSearch(ctx context.Context, episodeIDs []int) error {
	cmd := map[string]any{"name": "EpisodeSearch", "episodeIds": episodeIDs}
	return c.do(ctx, http.MethodPost, "/api/v3/command", nil, cmd, nil)
}
