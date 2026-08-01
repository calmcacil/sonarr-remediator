package sonarr

import (
	"context"
	"net/http"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ManualImport triggers a manual import for one file (POST /api/v3/manualimport).
//
// The request body follows types.ManualImportRequest; ImportMode is
// intentionally not sent — Sonarr uses its configured default (SPEC §12).
func (c *Client) ManualImport(ctx context.Context, req types.ManualImportRequest) error {
	return c.do(ctx, http.MethodPost, "/api/v3/manualimport", nil, req, nil)
}
