package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetQualityDefinitions fetches quality definitions with weights.
func (c *Client) GetQualityDefinitions(ctx context.Context) ([]types.QualityDefinition, error) {
	var defs []types.QualityDefinition
	if err := c.get(ctx, "/api/v3/qualitydefinition", &defs); err != nil {
		return nil, fmt.Errorf("get quality definitions: %w", err)
	}
	return defs, nil
}
