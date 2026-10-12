package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxResponse = 16 << 20

type APIError struct {
	Code int
	Kind string
}

func (e *APIError) Error() string { return fmt.Sprintf("Sonarr %s (HTTP %d)", e.Kind, e.Code) }

type Client struct {
	base   *url.URL
	key    string
	http   *http.Client
	dryRun bool
	cache  map[string]cachedRead
}
type cachedRead struct {
	value any
	err   error
}

func (c *Client) BeginCycle() { c.cache = make(map[string]cachedRead) }

func New(raw, key string, timeout time.Duration, dry bool) (*Client, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid Sonarr URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	h := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{base: u, key: key, http: h, dryRun: dry}, nil
}
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body, out any) error {
	if method != http.MethodGet && c.dryRun {
		return errors.New("dry-run mutation boundary rejected request")
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v3/" + path
	u.RawPath = ""
	u.RawQuery = q.Encode()
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return errors.New("invalid request body")
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if e != nil {
		return errors.New("cannot construct Sonarr request")
	}
	req.Header.Set("X-Api-Key", c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := c.http.Do(req)
	if e != nil {
		return errors.New("Sonarr connectivity or request timeout failure")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		kind := "request failed"
		if res.StatusCode == 401 || res.StatusCode == 403 {
			kind = "authentication failure"
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return &APIError{res.StatusCode, kind}
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, MaxResponse+1))
	if e != nil || len(b) > MaxResponse {
		return errors.New("incomplete or oversized Sonarr response")
	}
	if out == nil {
		if len(bytes.TrimSpace(b)) > 0 && !json.Valid(b) {
			return errors.New("malformed mutation acknowledgement")
		}
		return nil
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("empty required Sonarr resource")
	}
	if e = json.Unmarshal(b, out); e != nil {
		return errors.New("malformed Sonarr resource")
	}
	return nil
}
func (c *Client) Version(ctx context.Context) (string, error) {
	var s struct {
		Version string `json:"version"`
	}
	e := c.request(ctx, "GET", "system/status", nil, nil, &s)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(s.Version, "4.") {
		return "", errors.New("unsupported Sonarr version: requires v4")
	}
	return s.Version, nil
}

type page[T any] struct {
	Page     *int `json:"page"`
	PageSize *int `json:"pageSize"`
	Total    *int `json:"totalRecords"`
	Records  []T  `json:"records"`
}

func pages[T any](ctx context.Context, c *Client, path string, q url.Values, maxPages int, id func(T) int) ([]T, error) {
	var all []T
	seen := map[int]bool{}
	total := -1
	for n := 1; n <= maxPages; n++ {
		q.Set("page", strconv.Itoa(n))
		q.Set("pageSize", "100")
		var p page[T]
		if e := c.request(ctx, "GET", path, q, nil, &p); e != nil {
			return nil, e
		}
		if p.Page == nil || *p.Page != n || p.PageSize == nil || *p.PageSize != 100 || p.Total == nil || *p.Total < 0 || *p.Total > maxPages*100 || p.Records == nil || len(p.Records) > 100 {
			return nil, errors.New("invalid Sonarr pagination")
		}
		if total < 0 {
			total = *p.Total
		}
		if total != *p.Total {
			return nil, errors.New("changing Sonarr pagination")
		}
		for _, r := range p.Records {
			rid := id(r)
			if rid <= 0 || seen[rid] {
				return nil, errors.New("invalid or duplicated Sonarr identity")
			}
			seen[rid] = true
			all = append(all, r)
		}
		if len(all) > total {
			return nil, errors.New("inconsistent Sonarr total")
		}
		if len(all) == total {
			return all, nil
		}
		if len(p.Records) != 100 {
			return nil, errors.New("incomplete Sonarr page")
		}
	}
	return nil, errors.New("Sonarr pagination limit exceeded")
}
func (c *Client) Queue(ctx context.Context) ([]QueueRow, error) {
	var e error
	for i := 0; i < 3; i++ {
		var rows []QueueRow
		rows, e = pages(ctx, c, "queue", url.Values{"includeUnknownSeriesItems": {"true"}, "sortKey": {"timeleft"}, "sortDirection": {"ascending"}}, 1000, func(q QueueRow) int { return q.ID })
		if e == nil {
			for _, row := range rows {
				if row.SeriesID != nil && *row.SeriesID < 0 || row.EpisodeID != nil && *row.EpisodeID < 0 {
					return nil, errors.New("invalid queue target identity")
				}
				if strings.TrimSpace(row.DownloadID) == "" {
					pending := row.TrackedState == "" && (row.Status == "delay" || row.Status == "downloadClientUnavailable" || row.Status == "fallback")
					if !pending {
						return nil, errors.New("missing tracked queue download identity")
					}
				}
			}
			return rows, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, e
}
func (c *Client) Preview(ctx context.Context, id string) ([]Preview, error) {
	var p []Preview
	e := c.request(ctx, "GET", "manualimport", url.Values{"downloadId": {id}, "filterExistingFiles": {"false"}}, nil, &p)
	if e == nil && p == nil {
		e = errors.New("null preview")
	}
	return p, e
}
func resource[T any](ctx context.Context, c *Client, path string, id int, getID func(T) int) (T, error) {
	key := path + "/" + strconv.Itoa(id)
	if hit, ok := c.cache[key]; ok {
		return hit.value.(T), hit.err
	}
	var v T
	e := c.request(ctx, "GET", path+"/"+strconv.Itoa(id), nil, nil, &v)
	if e == nil && (id <= 0 || getID(v) != id) {
		e = errors.New("mismatched required Sonarr identity")
	}
	if c.cache != nil {
		c.cache[key] = cachedRead{value: v, err: e}
	}
	return v, e
}
func (c *Client) Episode(ctx context.Context, id int) (Episode, error) {
	return resource(ctx, c, "episode", id, func(e Episode) int { return e.ID })
}
func (c *Client) EpisodeFile(ctx context.Context, id int) (EpisodeFile, error) {
	return resource(ctx, c, "episodefile", id, func(e EpisodeFile) int { return e.ID })
}
func (c *Client) Series(ctx context.Context, id int) (Series, error) {
	return resource(ctx, c, "series", id, func(e Series) int { return e.ID })
}
func (c *Client) Profile(ctx context.Context, id int) (Profile, error) {
	return resource(ctx, c, "qualityprofile", id, func(e Profile) int { return e.ID })
}
func (c *Client) Command(ctx context.Context, id int) (Command, error) {
	return resource(ctx, c, "command", id, func(e Command) int { return e.ID })
}
func (c *Client) Media(ctx context.Context) (MediaManagement, error) {
	if hit, ok := c.cache["config/mediamanagement"]; ok {
		return hit.value.(MediaManagement), hit.err
	}
	var m MediaManagement
	e := c.request(ctx, "GET", "config/mediamanagement", nil, nil, &m)
	if c.cache != nil {
		c.cache["config/mediamanagement"] = cachedRead{value: m, err: e}
	}
	return m, e
}
func (c *Client) History(ctx context.Context, id string) ([]History, error) {
	h, e := pages(ctx, c, "history", url.Values{"downloadId": {id}, "sortKey": {"date"}, "sortDirection": {"descending"}}, 100, func(h History) int { return h.ID })
	if e == nil {
		for _, r := range h {
			if r.DownloadID != id {
				return nil, errors.New("history download identity mismatch")
			}
		}
	}
	return h, e
}
func (c *Client) Blocklist(ctx context.Context) ([]Blocklist, error) {
	return pages(ctx, c, "blocklist", url.Values{"sortKey": {"date"}, "sortDirection": {"descending"}}, 100, func(b Blocklist) int { return b.ID })
}
func (c *Client) Submit(ctx context.Context, body any) (Command, error) {
	var cmd Command
	e := c.request(ctx, "POST", "command", nil, body, &cmd)
	if e == nil && cmd.ID <= 0 {
		e = errors.New("missing command acknowledgement identity")
	}
	return cmd, e
}
func (c *Client) Remove(ctx context.Context, id int, blocklist bool) error {
	return c.request(ctx, "DELETE", "queue/"+strconv.Itoa(id), url.Values{"removeFromClient": {"true"}, "blocklist": {strconv.FormatBool(blocklist)}, "skipRedownload": {"true"}, "changeCategory": {"false"}}, nil, nil)
}
