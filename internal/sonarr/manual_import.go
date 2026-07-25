package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ManualImport executes a manual import via Sonarr's API.
func (c *Client) ManualImport(ctx context.Context, req types.ManualImportRequest) error {
	if err := c.post(ctx, "/api/v3/manualimport", req); err != nil {
		return fmt.Errorf("manual import: %w", err)
	}
	return nil
}
