package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetLanguages fetches language profiles.
func (c *Client) GetLanguages(ctx context.Context) ([]types.Language, error) {
	var langs []types.Language
	if err := c.get(ctx, "/api/v3/language", &langs); err != nil {
		return nil, fmt.Errorf("get languages: %w", err)
	}
	return langs, nil
}
