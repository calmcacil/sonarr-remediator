package sonarr

import (
	"context"
	"net/http"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetLanguages returns Sonarr's language profiles (GET /api/v3/language).
func (c *Client) GetLanguages(ctx context.Context) ([]types.Language, error) {
	var langs []types.Language
	if err := c.do(ctx, http.MethodGet, "/api/v3/language", nil, nil, &langs); err != nil {
		return nil, err
	}
	return langs, nil
}
