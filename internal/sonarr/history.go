package sonarr

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetHistory returns paged history records (GET /api/v3/history).
//
// Zero-valued optional parameters (eventType, seriesId, episodeId) are
// omitted from the query so Sonarr applies its defaults.
func (c *Client) GetHistory(ctx context.Context, params types.HistoryParams) ([]types.HistoryItem, error) {
	q := url.Values{}
	if params.Page != 0 {
		q.Set("page", strconv.Itoa(params.Page))
	}
	if params.PageSize != 0 {
		q.Set("pageSize", strconv.Itoa(params.PageSize))
	}
	if params.SortKey != "" {
		q.Set("sortKey", params.SortKey)
	}
	if params.SortDirection != "" {
		q.Set("sortDirection", params.SortDirection)
	}
	if params.EventType != 0 {
		q.Set("eventType", strconv.Itoa(params.EventType))
	}
	if params.SeriesID != 0 {
		q.Set("seriesId", strconv.Itoa(params.SeriesID))
	}
	if params.EpisodeID != 0 {
		q.Set("episodeId", strconv.Itoa(params.EpisodeID))
	}
	var items []types.HistoryItem
	if err := c.do(ctx, http.MethodGet, "/api/v3/history", q, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}
