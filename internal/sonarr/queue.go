package sonarr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetQueue returns the current download queue (GET /api/v3/queue).
//
// Sonarr serves the queue as a paged envelope (SPEC §12): one page of up to
// 1000 items is requested and the records are unwrapped. A queue larger than
// that is out of scope for a sidecar remediator.
func (c *Client) GetQueue(ctx context.Context) ([]types.QueueItem, error) {
	q := url.Values{"page": {"1"}, "pageSize": {"1000"}}
	var page types.Page[types.QueueItem]
	if err := c.do(ctx, http.MethodGet, "/api/v3/queue", q, nil, &page); err != nil {
		return nil, err
	}
	return page.Records, nil
}

// RemoveQueueItem removes a queue item (DELETE /api/v3/queue/{id}).
//
// When blocklist is true, the blocking parameter name is version-dependent
// (SPEC §12): v3 uses "blocklist=true" while major version >= 4 uses
// "removeFromClient=true".
func (c *Client) RemoveQueueItem(ctx context.Context, id int, blocklist bool) error {
	q := url.Values{}
	if blocklist {
		if c.MajorVersion() >= 4 {
			q.Set("removeFromClient", "true")
		} else {
			q.Set("blocklist", "true")
		}
	}
	path := fmt.Sprintf("/api/v3/queue/%d", id)
	return c.do(ctx, http.MethodDelete, path, q, nil, nil)
}
