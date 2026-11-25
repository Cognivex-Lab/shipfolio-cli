// Package client talks to the Shipfolio REST API.
//
// Every response is {"status":"ok","data":…} or
// {"status":"error","error":{"code","message","retryable"}}. A metric Google
// did not report is null, never 0, so the types here use pointers for
// metric values and callers must keep that distinction.
package client

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

// Client sends authenticated requests with an API key or an OAuth access
// token.
type Client struct {
	BaseURL   string // e.g. https://api.shipfolio.ai
	Key       string // sf_live_…
	UserAgent string
	HTTP      *http.Client
	// Token, when set, supplies the bearer token instead of Key: an OAuth
	// access token (sfo_…). After a 401 it is called once more with
	// renew set, so it can refresh a token the server no longer accepts.
	Token func(ctx context.Context, renew bool) (string, error)
	// Sleep waits before retrying a rate-limited request. Tests replace it.
	Sleep func(context.Context, time.Duration) error
}

// Error is an error response from the API, or a response that was not one.
type Error struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (" + e.Code + ")"
}

// maxRetries bounds automatic retries of rate_limited responses.
const maxRetries = 2

// maxWait is the longest Retry-After the client waits out on its own.
const maxWait = 30 * time.Second

// Do sends a request and returns the raw response body. On an error
// response it returns the body as well as an *Error, so callers that print
// raw output can still show what the server said.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	token, err := c.bearer(ctx, false)
	if err != nil {
		return nil, err
	}
	renewed := false
	for attempt := 0; ; attempt++ {
		raw, wait, err := c.once(ctx, method, u, token, payload)
		var ae *Error
		if c.Token != nil && !renewed && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			renewed = true
			if token, err = c.bearer(ctx, true); err != nil {
				return raw, err
			}
			attempt--
			continue
		}
		if wait <= 0 || attempt >= maxRetries {
			return raw, err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return raw, err
		}
	}
}

func (c *Client) bearer(ctx context.Context, renew bool) (string, error) {
	if c.Token == nil {
		return c.Key, nil
	}
	return c.Token(ctx, renew)
}

// once sends one request. A positive wait means the request was rate
// limited and may be retried after that long.
func (c *Client) once(ctx context.Context, method, u, token string, payload []byte) ([]byte, time.Duration, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, 0, nil
	}
	apiErr := decodeError(resp.StatusCode, raw)
	var wait time.Duration
	if apiErr.Code == "rate_limited" {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			wait = time.Duration(s) * time.Second
			if wait == 0 {
				wait = time.Second
			}
			if wait > maxWait {
				wait = 0
			}
		}
	}
	return raw, wait, apiErr
}

func decodeError(status int, raw []byte) *Error {
	var env struct {
		Error *struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error != nil && env.Error.Code != "" {
		return &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message, Retryable: env.Error.Retryable}
	}
	return &Error{Status: status, Message: fmt.Sprintf("the server answered %d %s", status, http.StatusText(status)), Retryable: status >= 500}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Call sends a request and decodes the whole response envelope into out.
func (c *Client) Call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	raw, err := c.Do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected response from %s: %w", path, err)
	}
	return nil
}

// Envelope is a successful response.
type Envelope[T any] struct {
	Data T `json:"data"`
}

type Whoami struct {
	WorkspaceID   string   `json:"workspace_id"`
	WorkspaceName string   `json:"workspace_name"`
	Plan          string   `json:"plan"`
	Scopes        []string `json:"scopes"`
}

type Source struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	ExternalID   string `json:"external_id"`
	Status       string `json:"status"`
	LastSyncedAt string `json:"last_synced_at"`
}

type Product struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Domain  string   `json:"domain"`
	Tags    []string `json:"tags"`
	Sources []Source `json:"sources"`
}

// Summary holds totals over a range. A nil field means Google reported
// nothing, not zero.
type Summary struct {
	Clicks         *float64 `json:"clicks"`
	Impressions    *float64 `json:"impressions"`
	CTR            *float64 `json:"ctr"`
	AvgPosition    *float64 `json:"avg_position"`
	Sessions       *float64 `json:"sessions"`
	Users          *float64 `json:"users"`
	EngagementRate *float64 `json:"engagement_rate"`
	KeyEvents      *float64 `json:"key_events"`
	// Bing Webmaster Tools. Bing reports no position for site totals, so
	// BingAvgPosition is set only on query and page rows.
	BingClicks      *float64 `json:"bing_clicks"`
	BingImpressions *float64 `json:"bing_impressions"`
	BingCTR         *float64 `json:"bing_ctr"`
	BingAvgPosition *float64 `json:"bing_avg_position"`
}

type Period struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	PrevStart string `json:"prev_start"`
	PrevEnd   string `json:"prev_end"`
	Days      int    `json:"days"`
	// Compare is "previous" (the days just before) or "year" (the same
	// days a year earlier).
	Compare string `json:"compare"`
}

type ProductOverview struct {
	Product
	Current  Summary             `json:"current"`
	Previous Summary             `json:"previous"`
	Change   map[string]*float64 `json:"change"`
}

type Overview struct {
	HasData  bool              `json:"has_data"`
	Period   Period            `json:"period"`
	Products []ProductOverview `json:"products"`
}

type OverviewResponse struct {
	Data Overview `json:"data"`
	AsOf *string  `json:"as_of"`
}

type Point struct {
	Date    string              `json:"date"`
	Values  map[string]*float64 `json:"values"`
	IsFinal bool                `json:"is_final"`
}

type Series struct {
	ProductID string  `json:"product_id"`
	Points    []Point `json:"points"`
}

type MetricsResponse struct {
	Data  []Series `json:"data"`
	Start string   `json:"start"`
	End   string   `json:"end"`
}

// Breakdown rows are kept as raw fields: each holds "value" plus whichever
// metrics apply to the dimension.
type Breakdown struct {
	ProductID string                       `json:"product_id"`
	Dimension string                       `json:"dimension"`
	SortedBy  string                       `json:"sorted_by"`
	Start     string                       `json:"start"`
	End       string                       `json:"end"`
	Rows      []map[string]json.RawMessage `json:"rows"`
	Note      string                       `json:"note"`
}

type Alert struct {
	ID        string  `json:"id"`
	ProductID string  `json:"product_id"`
	Rule      string  `json:"rule"`
	Metric    string  `json:"metric"`
	Date      string  `json:"date"`
	Value     float64 `json:"value"`
	Baseline  float64 `json:"baseline"`
	Message   string  `json:"message"`
}

type Connection struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

type Resource struct {
	ExternalID string `json:"external_id"`
	Label      string `json:"label"`
	Permission string `json:"permission"`
}

// Mover is one dimension value whose metric rose or fell between the
// previous range and the current one.
type Mover struct {
	Value    string   `json:"value"`
	Current  float64  `json:"current"`
	Previous float64  `json:"previous"`
	Delta    float64  `json:"delta"`
	Change   *float64 `json:"change"` // relative; null when it was 0 before
}

type Movers struct {
	ProductID string  `json:"product_id"`
	Dimension string  `json:"dimension"`
	Metric    string  `json:"metric"`
	Period    Period  `json:"period"`
	Gainers   []Mover `json:"gainers"`
	Losers    []Mover `json:"losers"`
}

// Annotation is a dated note on the charts. An empty ProductID means it
// applies to every product.
type Annotation struct {
	ID        string `json:"id"`
	ProductID string `json:"product_id"`
	Date      string `json:"date"`
	Text      string `json:"text"`
	CreatedBy string `json:"created_by"`
}

// ImportSite is a Search Console site that bulk import would turn into a
// product, with the GA4 property whose web stream is on the same domain.
type ImportSite struct {
	ExternalID string            `json:"external_id"`
	Domain     string            `json:"domain"`
	Name       string            `json:"name"`
	Exists     bool              `json:"exists"`
	GA4        map[string]string `json:"ga4"` // external_id, label, connection_id; nil when none matched
}

type ImportPreview struct {
	Sites    []ImportSite `json:"sites"`
	Plan     string       `json:"plan"`
	Limit    int          `json:"limit"`
	Products int          `json:"products"`
}

type ImportedProduct struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	GA4    bool   `json:"ga4"`
}

type ImportResult struct {
	Created []ImportedProduct `json:"created"`
}
