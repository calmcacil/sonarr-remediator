package sonarr

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetHistory fetches history events.
func (c *Client) GetHistory(ctx context.Context, params types.HistoryParams) ([]types.HistoryItem, error) {
	type historyRecord struct {
		ID          int               `json:"id"`
		SeriesID    int               `json:"seriesId"`
		EpisodeID   int               `json:"episodeId"`
		SourceTitle string            `json:"sourceTitle"`
		EventType   string            `json:"eventType"`
		Quality     types.QualityModel `json:"quality"`
		Date        time.Time         `json:"date"`
		Data        map[string]string `json:"data"`
	}
	type historyResponse struct {
		Records []historyRecord `json:"records"`
	}

	u, _ := url.Parse("/api/v3/history")
	q := u.Query()

	if params.Page > 0 {
		q.Set("page", strconv.Itoa(params.Page))
	} else {
		q.Set("page", "1")
	}
	if params.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(params.PageSize))
	} else {
		q.Set("pageSize", "100")
	}
	if params.SortKey != "" {
		q.Set("sortKey", params.SortKey)
	} else {
		q.Set("sortKey", "date")
	}
	if params.SortDirection != "" {
		q.Set("sortDirection", params.SortDirection)
	} else {
		q.Set("sortDirection", "descending")
	}
	if params.EventType > 0 {
		q.Set("eventType", strconv.Itoa(params.EventType))
	}
	if params.SeriesID > 0 {
		q.Set("seriesId", strconv.Itoa(params.SeriesID))
	}
	if params.EpisodeID > 0 {
		q.Set("episodeId", strconv.Itoa(params.EpisodeID))
	}

	u.RawQuery = q.Encode()

	var resp historyResponse
	if err := c.getURL(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}

	items := make([]types.HistoryItem, len(resp.Records))
	for i, r := range resp.Records {
		items[i] = types.HistoryItem{
			ID:          r.ID,
			SeriesID:    r.SeriesID,
			EpisodeID:   r.EpisodeID,
			SourceTitle: r.SourceTitle,
			EventType:   r.EventType,
			Quality:     r.Quality.Quality.Name,
			Date:        r.Date,
			Data:        r.Data,
		}
	}
	return items, nil
}
