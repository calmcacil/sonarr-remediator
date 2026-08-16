package sonarr

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetEpisode returns one episode (GET /api/v3/episode/{id}).
func (c *Client) GetEpisode(ctx context.Context, episodeID int) (*types.EpisodeResource, error) {
	var ep types.EpisodeResource
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v3/episode/%d", episodeID), nil, nil, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// GetEpisodeFile returns one episode file (GET /api/v3/episodefile/{id}).
func (c *Client) GetEpisodeFile(ctx context.Context, episodeFileID int) (*types.EpisodeFileResource, error) {
	var ef types.EpisodeFileResource
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v3/episodefile/%d", episodeFileID), nil, nil, &ef); err != nil {
		return nil, err
	}
	return &ef, nil
}

// GetEpisodeFileForEpisode fetches the episode, checks hasFile, and if true
// fetches the episode file. Returns (nil, nil) when the episode has no file.
func (c *Client) GetEpisodeFileForEpisode(ctx context.Context, episodeID int) (*types.EpisodeFileResource, error) {
	ep, err := c.GetEpisode(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	if !ep.HasFile || ep.EpisodeFileID <= 0 {
		return nil, nil
	}
	return c.GetEpisodeFile(ctx, ep.EpisodeFileID)
}

// GetSeries returns one series (GET /api/v3/series/{id}).
func (c *Client) GetSeries(ctx context.Context, seriesID int) (*types.SeriesResource, error) {
	var s types.SeriesResource
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v3/series/%d", seriesID), nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadDefinitions fetches quality definitions and languages once and caches
// them on the client for QualityWeightByID/QualityWeightByName lookups.
// Concurrent callers are safe: after the first successful load, subsequent
// calls return immediately with the cached data.
func (c *Client) LoadDefinitions(ctx context.Context) error {
	c.mu.RLock()
	loaded := c.qualityByID != nil && c.languages != nil
	c.mu.RUnlock()
	if loaded {
		return nil
	}

	qualities, err := c.GetQualityDefinitions(ctx)
	if err != nil {
		return err
	}
	languages, err := c.GetLanguages(ctx)
	if err != nil {
		return err
	}

	byID := make(map[int]types.QualityDefinition, len(qualities))
	byName := make(map[string]types.QualityDefinition, len(qualities)*2)
	for _, q := range qualities {
		byID[q.ID] = q
		if q.Name != "" {
			byName[strings.ToLower(q.Name)] = q
		}
		if q.Title != "" {
			byName[strings.ToLower(q.Title)] = q
		}
	}

	c.mu.Lock()
	if c.qualityByID == nil && c.languages == nil {
		c.qualityByID = byID
		c.qualityByName = byName
		c.languages = languages
	}
	c.mu.Unlock()
	return nil
}
