package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultPageSize = 100
	maxRetries      = 3
)

// Client handles all Sonarr API communication.
type Client struct {
	BaseURL    *url.URL
	APIKey     string
	HTTPClient *http.Client
	Version    string // detected Sonarr version, e.g. "4.0.0.741"
	sem        chan struct{}
}

// New creates a new Sonarr API client.
func New(baseURL string, apiKey string, timeout time.Duration, maxConcurrency int) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid sonarr URL: %w", err)
	}
	return &Client{
		BaseURL: u,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
		sem: make(chan struct{}, maxConcurrency),
	}, nil
}

// DetectVersion fetches system status and extracts the Sonarr version.
func (c *Client) DetectVersion(ctx context.Context) error {
	status, err := c.GetSystemStatus(ctx)
	if err != nil {
		return fmt.Errorf("detecting sonarr version: %w", err)
	}
	c.Version = status.Version
	return nil
}

// majorVersion returns the major version number.
func (c *Client) MajorVersion() int {
	parts := strings.Split(c.Version, ".")
	if len(parts) > 0 {
		var v int
		fmt.Sscanf(parts[0], "%d", &v)
		return v
	}
	return 3 // default to v3
}

// do performs an HTTP request with retry and backoff.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	// Acquire semaphore
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var ref *url.URL
	if idx := strings.Index(path, "?"); idx >= 0 {
		ref = &url.URL{Path: path[:idx], RawQuery: path[idx+1:]}
	} else {
		ref = &url.URL{Path: path}
	}
	u := c.BaseURL.ResolveReference(ref)

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
			select {
			case <-time.After(backoff + jitter):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}

		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			resp.Body.Close()
			return nil, fmt.Errorf("authentication failed (%d): check API key", resp.StatusCode)
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error (%d)", resp.StatusCode)
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

// get performs a GET request and decodes the JSON response.
func (c *Client) get(ctx context.Context, path string, result any) error {
	resp, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sonarr API error (%d): %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}



// getURL performs a GET request from a pre-built URL and decodes the JSON response.
func (c *Client) getURL(ctx context.Context, u *url.URL, result any) error {
	resp, err := c.do(ctx, "GET", u.RequestURI(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sonarr API error (%d): %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// post performs a POST request with JSON body.
func (c *Client) post(ctx context.Context, path string, body any) error {
	resp, err := c.do(ctx, "POST", path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sonarr API error (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// delete performs a DELETE request.
func (c *Client) delete(ctx context.Context, path string) error {
	resp, err := c.do(ctx, "DELETE", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sonarr API error (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}