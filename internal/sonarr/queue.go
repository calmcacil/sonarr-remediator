package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

type queueResponse struct {
	Records []types.QueueItem `json:"records"`
}

// GetQueue fetches the download queue including unknown series items.
func (c *Client) GetQueue(ctx context.Context) ([]types.QueueItem, error) {
	var resp queueResponse
	if err := c.get(ctx, "/api/v3/queue?includeUnknownSeriesItems=true", &resp); err != nil {
		return nil, fmt.Errorf("get queue: %w", err)
	}
	return resp.Records, nil
}

type queueDetailResponse struct {
	Records []types.QueueDetailItem `json:"records"`
}

// GetQueueDetails fetches queue with episode details.
func (c *Client) GetQueueDetails(ctx context.Context) ([]types.QueueDetailItem, error) {
	var resp queueDetailResponse
	if err := c.get(ctx, "/api/v3/queue/details?includeUnknownSeriesItems=true", &resp); err != nil {
		return nil, fmt.Errorf("get queue details: %w", err)
	}
	return resp.Records, nil
}

// RemoveQueueItem removes a queue item. Uses blocklist=true for v3, removeFromClient=true for v4+.
func (c *Client) RemoveQueueItem(ctx context.Context, id int, blocklist bool) error {
	var path string
	if c.MajorVersion() < 4 {
		path = fmt.Sprintf("/api/v3/queue/%d?blocklist=%t", id, blocklist)
	} else {
		path = fmt.Sprintf("/api/v3/queue/%d?removeFromClient=true", id)
	}
	return c.delete(ctx, path)
}
