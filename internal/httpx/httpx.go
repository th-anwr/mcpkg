// Package httpx is a polite HTTP client: per-host rate limiting, retries with
// backoff, Retry-After / GitHub rate-limit awareness and response size caps.
package httpx

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
	"sync"
	"time"

	"github.com/mcpkg/mcpkg/internal/queue"
)

// Client is safe for concurrent use.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	MaxBody   int64
	Retries   int

	mu       sync.Mutex
	nextSlot map[string]time.Time
	minGap   map[string]time.Duration
}

// New creates a client. Per-host minimum gaps protect upstream APIs.
func New(userAgent string) *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		UserAgent: userAgent,
		MaxBody:   64 << 20,
		Retries:   3,
		nextSlot:  map[string]time.Time{},
		minGap: map[string]time.Duration{
			"crates.io":      1100 * time.Millisecond, // crates.io crawler policy: 1 req/s
			"pypi.org":       50 * time.Millisecond,
			"api.github.com": 50 * time.Millisecond,
		},
	}
}

// SetRate sets the minimum gap between requests to a host.
func (c *Client) SetRate(host string, gap time.Duration) {
	c.mu.Lock()
	c.minGap[host] = gap
	c.mu.Unlock()
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	URL    string
	Body   string
}

func (e *HTTPError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300]
	}
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.URL, e.Status, b)
}

// IsNotFound reports a 404/410.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == 404 || he.Status == 410)
}

// Request describes one call.
type Request struct {
	Method  string
	URL     string
	Header  map[string]string
	Body    any // marshalled as JSON when non-nil
	MaxBody int64
}

// Do performs a request with retries and returns the body.
func (c *Client) Do(ctx context.Context, r Request) ([]byte, http.Header, error) {
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, nil, err
	}
	var payload []byte
	if r.Body != nil {
		if payload, err = json.Marshal(r.Body); err != nil {
			return nil, nil, err
		}
	}
	maxBody := r.MaxBody
	if maxBody <= 0 {
		maxBody = c.MaxBody
	}
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if err := c.wait(ctx, u.Hostname()); err != nil {
			return nil, nil, err
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", c.UserAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range r.Header {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			sleep(ctx, backoff(attempt))
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			sleep(ctx, backoff(attempt))
			continue
		}
		if int64(len(data)) > maxBody {
			return nil, resp.Header, fmt.Errorf("response from %s exceeds %d bytes", u.Host, maxBody)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return data, resp.Header, nil
		}
		herr := &HTTPError{Status: resp.StatusCode, URL: r.URL, Body: string(data)}
		// Rate limits: surface as ErrRateLimited so the queue can retry later without burning attempts.
		if until, limited := rateLimitedUntil(resp); limited {
			if time.Until(until) < 90*time.Second && attempt < c.Retries {
				sleep(ctx, time.Until(until)+time.Second)
				continue
			}
			return nil, resp.Header, &queue.ErrRateLimited{Until: until}
		}
		if resp.StatusCode >= 500 || resp.StatusCode == 408 {
			lastErr = herr
			sleep(ctx, backoff(attempt))
			continue
		}
		return nil, resp.Header, herr
	}
	return nil, nil, lastErr
}

// GetJSON fetches and decodes JSON.
func (c *Client) GetJSON(ctx context.Context, rawURL string, header map[string]string, out any) error {
	h := map[string]string{"Accept": "application/json"}
	for k, v := range header {
		h[k] = v
	}
	data, _, err := c.Do(ctx, Request{URL: rawURL, Header: h})
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// PostJSON posts JSON and decodes the JSON response.
func (c *Client) PostJSON(ctx context.Context, rawURL string, header map[string]string, in, out any) error {
	h := map[string]string{"Accept": "application/json"}
	for k, v := range header {
		h[k] = v
	}
	data, _, err := c.Do(ctx, Request{Method: http.MethodPost, URL: rawURL, Header: h, Body: in})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) wait(ctx context.Context, host string) error {
	c.mu.Lock()
	gap := c.minGap[host]
	now := time.Now()
	slot := c.nextSlot[host]
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot[host] = slot.Add(gap)
	c.mu.Unlock()
	if d := time.Until(slot); d > 0 {
		return sleep(ctx, d)
	}
	return nil
}

func rateLimitedUntil(resp *http.Response) (time.Time, bool) {
	if resp.StatusCode != 429 && resp.StatusCode != 403 {
		return time.Time{}, false
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(reset, 0), true
		}
		return time.Now().Add(time.Minute), true
	}
	if resp.StatusCode == 429 {
		return time.Now().Add(time.Minute), true
	}
	return time.Time{}, false
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
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
