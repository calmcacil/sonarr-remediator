package sonarr

import (
	"context"
	"fmt"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// GetDownloadClients fetches download client configurations.
func (c *Client) GetDownloadClients(ctx context.Context) ([]types.DownloadClientResource, error) {
	var clients []types.DownloadClientResource
	if err := c.get(ctx, "/api/v3/downloadclient", &clients); err != nil {
		return nil, fmt.Errorf("get download clients: %w", err)
	}
	return clients, nil
}

// GetEpisode fetches a single episode.
func (c *Client) GetEpisode(ctx context.Context, episodeID int) (*types.EpisodeResource, error) {
	var ep types.EpisodeResource
	if err := c.get(ctx, fmt.Sprintf("/api/v3/episode/%d", episodeID), &ep); err != nil {
		return nil, fmt.Errorf("get episode %d: %w", episodeID, err)
	}
	return &ep, nil
}

// GetEpisodeFile fetches an episode file.
func (c *Client) GetEpisodeFile(ctx context.Context, episodeFileID int) (*types.EpisodeFileResource, error) {
	var ef types.EpisodeFileResource
	if err := c.get(ctx, fmt.Sprintf("/api/v3/episodefile/%d", episodeFileID), &ef); err != nil {
		return nil, fmt.Errorf("get episode file %d: %w", episodeFileID, err)
	}
	return &ef, nil
}

// GetEpisodeFileForEpisode fetches the episode, checks hasFile,
// and if true, fetches the episode file. Returns nil if no file exists.
func (c *Client) GetEpisodeFileForEpisode(ctx context.Context, episodeID int) (*types.EpisodeFileResource, error) {
	ep, err := c.GetEpisode(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	if !ep.HasFile || ep.EpisodeFileID == 0 {
		return nil, nil
	}
	return c.GetEpisodeFile(ctx, ep.EpisodeFileID)
}

// GetSeries fetches a single series.
func (c *Client) GetSeries(ctx context.Context, seriesID int) (*types.SeriesResource, error) {
	var series types.SeriesResource
	if err := c.get(ctx, fmt.Sprintf("/api/v3/series/%d", seriesID), &series); err != nil {
		return nil, fmt.Errorf("get series %d: %w", seriesID, err)
	}
	return &series, nil
}
