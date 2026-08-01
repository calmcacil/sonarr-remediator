package sonarr

import (
	"context"
	"net/http"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetQualityDefinitions returns Sonarr's quality definitions
// (GET /api/v3/qualitydefinition).
func (c *Client) GetQualityDefinitions(ctx context.Context) ([]types.QualityDefinition, error) {
	var defs []types.QualityDefinition
	if err := c.do(ctx, http.MethodGet, "/api/v3/qualitydefinition", nil, nil, &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// QualityWeightByID returns the weight of the quality definition with the
// given id from the cache populated by LoadDefinitions.
func (c *Client) QualityWeightByID(id int) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.qualityByID[id]
	if !ok {
		return 0, false
	}
	return q.Weight, true
}

// QualityWeightByName returns the weight of the quality definition matching
// the given name or title (case-insensitive) from the cache populated by
// LoadDefinitions.
func (c *Client) QualityWeightByName(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.qualityByName[strings.ToLower(name)]
	if !ok {
		return 0, false
	}
	return q.Weight, true
}
