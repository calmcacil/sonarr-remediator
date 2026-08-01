// Package sonarr implements the Sonarr v3 API client (SPEC §5.1).
//
// The client speaks the v3 API, which is used by both Sonarr v3 and v4
// installations. The running major version is detected at startup via
// DetectVersion and adapts the few calls that differ between versions
// (see §12 Appendix).
package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ErrAuth is returned when Sonarr rejects the credentials (401/403).
// Monitors stay disabled until credentials are fixed.
var ErrAuth = errors.New("sonarr: authentication failed")

// DefaultMaxConcurrency bounds concurrent in-flight requests when the
// constructor is called with a non-positive limit.
const DefaultMaxConcurrency = 5

const (
	// maxRetries is the number of retry attempts after the initial request
	// for transient failures (5xx, 429, network errors).
	maxRetries = 3
	// retryBaseDelay is the starting exponential backoff delay.
	retryBaseDelay = 500 * time.Millisecond
	// maxErrorBody caps how much of a non-success response body is retained
	// for error messages.
	maxErrorBody = 64 << 10
)

// Client is a Sonarr API client (SPEC §5.1).
type Client struct {
	BaseURL    *url.URL
	APIKey     string
	HTTPClient *http.Client
	Version    string // detected Sonarr version, e.g. "4.0.0.741"

	// sem limits concurrent in-flight requests.
	sem chan struct{}

	// mu guards the cached definitions below.
	mu            sync.RWMutex
	qualityByID   map[int]types.QualityDefinition
	qualityByName map[string]types.QualityDefinition
	languages     []types.Language
}

// New builds a client for the given base URL and API key. timeout bounds each
// individual HTTP request; maxConcurrency caps concurrent requests (default 5).
func New(baseURL, apiKey string, timeout time.Duration, maxConcurrency int) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("sonarr: base URL is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("sonarr: API key is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("sonarr: invalid base URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("sonarr: base URL must use http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("sonarr: base URL must include a host")
	}
	if maxConcurrency <= 0 {
		maxConcurrency = DefaultMaxConcurrency
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		BaseURL:    u,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: timeout},
		sem:        make(chan struct{}, maxConcurrency),
	}, nil
}

// DetectVersion fetches the system status and records the running Sonarr
// version (e.g. "4.0.0.741") on the client.
func (c *Client) DetectVersion(ctx context.Context) error {
	st, err := c.GetSystemStatus(ctx)
	if err != nil {
		return err
	}
	c.Version = st.Version
	return nil
}

// MajorVersion returns the major component of the detected Sonarr version.
// It defaults to 3 (the API baseline) when the version is unknown or the
// version string cannot be parsed.
func (c *Client) MajorVersion() int {
	v := c.Version
	if i := strings.IndexByte(v, '.'); i > 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 3 {
		return 3
	}
	return n
}

// httpError is a terminal (non-retryable) HTTP error response.
type httpError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("sonarr: %s (status %d)", e.Status, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// do executes a single API call with concurrency limiting and retry handling
// (SPEC §5.1):
//
//   - 401/403 → ErrAuth
//   - 429 and 5xx → transient, retried with exponential backoff + jitter
//   - other 4xx → terminal
//   - network errors → transient, retried
//
// body, when non-nil, is JSON-encoded as the request body. out, when non-nil,
// receives the JSON-decoded response body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	var reqBody []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("sonarr: encode request body: %w", err)
		}
		reqBody = b
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, path, query, reqBody)
		if err != nil {
			return err
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			if attempt >= maxRetries {
				return fmt.Errorf("sonarr: request %s %s failed: %w", method, path, err)
			}
			if err := c.sleep(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("sonarr: read %s %s response: %w", method, path, readErr)
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return ErrAuth
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
			lastErr = &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Body: snippet(respBody)}
			if attempt >= maxRetries {
				return fmt.Errorf("sonarr: request %s %s: %w", method, path, lastErr)
			}
			if err := c.sleep(ctx, attempt); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= http.StatusBadRequest:
			return &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Body: snippet(respBody)}
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("sonarr: decode %s %s response: %w", method, path, err)
			}
		}
		return nil
	}
}

// newRequest builds an authenticated request against the v3 API root.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	u := *c.BaseURL
	u.Path = strings.TrimSuffix(c.BaseURL.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("sonarr: build request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// sleep backs off with exponential delay plus jitter before a retry.
func (c *Client) sleep(ctx context.Context, attempt int) error {
	wait := backoffDelay(attempt)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffDelay returns the jittered delay for retry attempt (0-indexed):
// half of the exponential window plus uniform jitter over the other half,
// guaranteeing a non-zero wait.
func backoffDelay(attempt int) time.Duration {
	max := retryBaseDelay << attempt // 500ms, 1s, 2s
	half := max / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// snippet truncates an error body for inclusion in error messages.
func snippet(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}
