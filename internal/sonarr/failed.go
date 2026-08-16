package sonarr

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// MarkHistoryFailed marks a history item as failed
// (POST /api/v3/history/failed/{id}). This is Sonarr's native "blocklist and
// re-download" primitive: it adds the release to the blocklist and, when
// AutoRedownloadFailed is enabled (the default), triggers an EpisodeSearch
// command. It is the only working blocklist path for torrent clients whose
// reported hash differs from the grabbed history's (torboxarr-style bridges,
// SPEC §3.9) — the queue DELETE blocklist parameter silently no-ops there.
func (c *Client) MarkHistoryFailed(ctx context.Context, historyID int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v3/history/failed/%d", historyID), nil, nil, nil)
}

// FindGrabbedHistory locates the grabbed history item for a queue item so the
// executor can blocklist the release. Matching is by series/episode plus the
// release title: the queue's downloadId may differ from the history's
// downloadId (torboxarr-style bridges report a synthetic hash), so the
// downloadId is deliberately not used. The newest grabbed event whose
// sourceTitle matches the queue item's title (case-insensitive, extension
// stripped, prefix tolerated) is returned; nil when nothing matches.
func (c *Client) FindGrabbedHistory(ctx context.Context, seriesID, episodeID int, releaseTitle string) (*types.HistoryItem, error) {
	records, err := c.GetHistory(ctx, types.HistoryParams{
		Page:       1,
		PageSize:   50,
		SortKey:    "date",
		SortDirection: "descending",
		EventType:  1, // grabbed
		SeriesID:   seriesID,
		EpisodeID:  episodeID,
	})
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSuffix(filepath.Base(releaseTitle), filepath.Ext(releaseTitle)))
	for _, h := range records {
		src := strings.ToLower(strings.TrimSuffix(filepath.Base(h.SourceTitle), filepath.Ext(h.SourceTitle)))
		if src == want || strings.HasPrefix(src, want) || strings.HasPrefix(want, src) {
			return &h, nil
		}
	}
	return nil, nil
}
