package sonarr

import (
	"context"
	"fmt"
	"net/url"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Parse sends a path to Sonarr's parse endpoint for title parsing.
func (c *Client) Parse(ctx context.Context, path string) (*types.ParseResult, error) {
	u, _ := url.Parse("/api/v3/parse")
	q := u.Query()
	q.Set("path", path)
	u.RawQuery = q.Encode()

	var result types.ParseResult
	if err := c.get(ctx, u.String(), &result); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &result, nil
}
