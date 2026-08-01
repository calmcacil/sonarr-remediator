package sonarr

import (
	"context"
	"net/http"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetSystemStatus returns Sonarr's system status (GET /api/v3/system/status).
func (c *Client) GetSystemStatus(ctx context.Context) (types.SystemStatus, error) {
	var st types.SystemStatus
	if err := c.do(ctx, http.MethodGet, "/api/v3/system/status", nil, nil, &st); err != nil {
		return types.SystemStatus{}, err
	}
	return st, nil
}
