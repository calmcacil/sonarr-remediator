package sonarr

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Parse asks Sonarr to parse a file path or title (GET /api/v3/parse?path=…).
func (c *Client) Parse(ctx context.Context, path string) (*types.ParseResult, error) {
	q := url.Values{}
	q.Set("path", path)
	var res types.ParseResult
	if err := c.do(ctx, http.MethodGet, "/api/v3/parse", q, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// PathTranslator maps file paths between the agent's mount view and Sonarr's
// mount view (SPEC §1). When both roots are set it swaps the leading prefix
// agentRoot<->sonarrRoot; otherwise it is the identity.
type PathTranslator struct {
	agentRoot  string
	sonarrRoot string
}

// NewPathTranslator creates a translator between the agent and Sonarr roots.
// Empty roots disable translation.
func NewPathTranslator(agentRoot, sonarrRoot string) *PathTranslator {
	return &PathTranslator{
		agentRoot:  strings.TrimSuffix(agentRoot, "/"),
		sonarrRoot: strings.TrimSuffix(sonarrRoot, "/"),
	}
}

// ToSonarr converts an agent-visible path to Sonarr's view of the filesystem.
func (t *PathTranslator) ToSonarr(agentPath string) string {
	if t.agentRoot == "" || t.sonarrRoot == "" {
		return agentPath
	}
	return swapPrefix(agentPath, t.agentRoot, t.sonarrRoot)
}

// ToAgent converts a Sonarr-visible path to the agent's mount view.
func (t *PathTranslator) ToAgent(sonarrPath string) string {
	if t.agentRoot == "" || t.sonarrRoot == "" {
		return sonarrPath
	}
	return swapPrefix(sonarrPath, t.sonarrRoot, t.agentRoot)
}

// swapPrefix replaces a leading path prefix, guarding against partial
// directory-name matches (e.g. /data must not match /databases).
func swapPrefix(p, from, to string) string {
	switch {
	case p == from:
		return to
	case strings.HasPrefix(p, from+"/"):
		return to + strings.TrimPrefix(p, from)
	default:
		return p
	}
}
