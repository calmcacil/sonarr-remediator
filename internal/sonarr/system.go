package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetSystemStatus fetches Sonarr's system status including version.
func (c *Client) GetSystemStatus(ctx context.Context) (*types.SystemStatus, error) {
	var status types.SystemStatus
	if err := c.get(ctx, "/api/v3/system/status", &status); err != nil {
		return nil, fmt.Errorf("get system status: %w", err)
	}
	return &status, nil
}
