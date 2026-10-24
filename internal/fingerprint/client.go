// Package fingerprint talks to the Fingerprinter and turns a render into what
// the inventory keeps: a normalized observation, technologies with parsed
// versions, and a screenshot.
package fingerprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Result is the Fingerprinter's answer to POST /scan.
type Result struct {
	URL           string         `json:"url"`
	Chain         []Hop          `json:"chain"`
	Technologies  []Technology   `json:"technologies"`
	Cookies       map[string]any `json:"cookies"`
	Metadata      *Metadata      `json:"metadata"`
	ExternalHosts []string       `json:"external_hosts"`
	WebSockets    []string       `json:"web_sockets"`
	Scripts       []Script       `json:"scripts"`
	Screenshot    *string        `json:"screenshot"`
	ScannedAt     string         `json:"scanned_at"`
	Version       string         `json:"version"`
}

// Hop is one response of the redirect chain.
type Hop struct {
	URL          string            `json:"url"`
	StatusCode   int               `json:"status_code"`
	Headers      map[string]string `json:"headers"`
	Title        *string           `json:"title"`
	ResponseSize int               `json:"response_size"`
	RemoteIP     string            `json:"remote_ip_address"`
}

// Technology is one detection, with the evidence it rests on.
type Technology struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	Category string          `json:"category"`
	CPE      string          `json:"cpe"`
	Proof    json.RawMessage `json:"proof"`
}

// Metadata is what the render found around the page.
type Metadata struct {
	RobotsTXT   bool    `json:"robots_txt"`
	LLMsTXT     bool    `json:"llms_txt"`
	Sitemap     *string `json:"sitemap"`
	Favicon     *string `json:"favicon"`
	FaviconURL  *string `json:"favicon_url"`
	FaviconHash *string `json:"favicon_hash"`
}

// Script is one loaded script.
type Script struct {
	URL      string `json:"url"`
	Internal bool   `json:"internal"`
	Hash     string `json:"hash"`
}

// Errors a render ends on, for the dispatcher to act on.
var (
	// ErrBusy is a 429: the service had no free slot. Nothing about the target.
	ErrBusy = errors.New("fingerprinter busy")
	// ErrUnreachable is the target not answering the browser: a measurement.
	ErrUnreachable = errors.New("target did not answer the browser")
)

// BusyError carries the Retry-After of a 429.
type BusyError struct{ RetryAfter time.Duration }

func (e *BusyError) Error() string {
	return fmt.Sprintf("fingerprinter busy, retry after %s", e.RetryAfter)
}
func (e *BusyError) Unwrap() error { return ErrBusy }

// Client calls one Fingerprinter.
type Client struct {
	url  string
	http *http.Client
}

// NewClient builds a client. timeout bounds one scan, the service's own
// budget plus a margin.
func NewClient(url string, timeout time.Duration) *Client {
	return &Client{url: strings.TrimSuffix(url, "/"), http: &http.Client{Timeout: timeout}}
}

// maxResult bounds one answer: a screenshot plus a page's worth of metadata.
const maxResult = 32 << 20

// Scan renders one URL, with a screenshot.
func (c *Client) Scan(ctx context.Context, url string) (*Result, []byte, error) {
	body, _ := json.Marshal(map[string]any{"url": url, "options": map[string]any{"screenshot": true}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/scan", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fingerprinter: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResult))
	if err != nil {
		return nil, nil, fmt.Errorf("fingerprinter: %w", err)
	}
	switch res.StatusCode {
	case http.StatusOK:
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, nil, fmt.Errorf("fingerprinter: decode: %w", err)
		}
		return &r, raw, nil
	case http.StatusTooManyRequests:
		secs, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		return nil, nil, &BusyError{RetryAfter: time.Duration(max(secs, 1)) * time.Second}
	case http.StatusRequestTimeout:
		return nil, nil, fmt.Errorf("%w: scan timed out", ErrUnreachable)
	case http.StatusInternalServerError:
		// The one 500 that is about the target: the browser got no response.
		if bytes.Contains(raw, []byte("no responses received")) {
			return nil, nil, fmt.Errorf("%w: no responses received", ErrUnreachable)
		}
	}
	return nil, nil, fmt.Errorf("fingerprinter: %s: %s", res.Status, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
}

// Health returns the service's version, or an error when it does not answer.
func (c *Client) Health(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/health", nil)
	if err != nil {
		return "", err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fingerprinter health: %s", res.Status)
	}
	var h struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&h); err != nil {
		return "", err
	}
	return h.Version, nil
}

// Major is a semantic version's major component, "v1.2.3" giving 1. ok is
// false for anything else, a commit hash or "main": such a version says
// nothing about what changed, so it cannot justify rendering everything again.
func Major(v string) (major int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	head, rest, dotted := strings.Cut(v, ".")
	if !dotted || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
