package fingerprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResultBytes bounds a render. A page with a large inline favicon and a long
// chain is still small; anything past this is a service the caller has no
// business believing.
const maxResultBytes = 16 << 20

// Saturated is the service saying its pool is full.
//
// It is a state of the service, so it must not touch the asset: no
// observation, no counter, no streak, no last_fingerprint_at. A render that
// reached the target and got nothing writes an observation; a render that never
// happened writes nothing at all, and confusing the two walks assets toward
// unobservable for a reason that has nothing to do with them.
type Saturated struct{ RetryAfter time.Duration }

func (s *Saturated) Error() string {
	return "the rendering service is saturated, retry after " + s.RetryAfter.String()
}

// ErrRefused is a target the service would not address at all: an unresolvable
// name, a scheme it does not speak, or a range it refuses. Like saturation it
// is a probe error and writes nothing.
var ErrRefused = errors.New("the rendering service refused the target")

// Client is the control plane's side of POST /scan.
type Client struct {
	base   string
	http   *http.Client
	guard  *Guard
	random func() float64
}

// New builds a client. The timeout is the caller's ceiling and is deliberately
// longer than the service's own: a render is seconds, and cutting it here would
// turn the service's answer into a caller-side error nobody can qualify.
func New(base string, timeout time.Duration, guard *Guard) *Client {
	if guard == nil {
		guard = NewGuard(nil)
	}
	return &Client{
		base:   strings.TrimSuffix(base, "/"),
		http:   &http.Client{Timeout: timeout},
		guard:  guard,
		random: rand.Float64,
	}
}

// Options are what a scan may be asked for.
type Options struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxRedirects   int `json:"max_redirects,omitempty"`
	// SkipPathChecks turns off robots.txt, the sitemap and the 404 probe. They
	// are part of what a render costs, so a pass that does not need them says
	// so rather than paying for them.
	SkipPathChecks bool `json:"skip_path_checks,omitempty"`
	// Screenshot is never asked for. The capture does not reach the database,
	// and nothing else stores it yet, so requesting one would spend bandwidth
	// on bytes with nowhere to go.
	Screenshot bool `json:"screenshot,omitempty"`
}

type request struct {
	URL     string  `json:"url"`
	Options Options `json:"options"`
}

// Scan renders one URL.
func (c *Client) Scan(ctx context.Context, target string, opts Options) (*Result, error) {
	if err := c.guard.Check(ctx, target); err != nil {
		return nil, err
	}

	body, err := json.Marshal(request{URL: target, Options: opts})
	if err != nil {
		return nil, fmt.Errorf("encode scan request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/scan", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build scan request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Saturated{RetryAfter: c.retryAfter(resp)}
	case resp.StatusCode >= 400:
		// The service could not address the target. That is a probe error and
		// not a measurement: a target that refuses, times out or answers a
		// challenge all come back as results.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: %s: %s", ErrRefused, resp.Status, bytes.TrimSpace(detail))
	}

	var result Result
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResultBytes)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode render: %w", err)
	}
	return &result, nil
}

// retryAfter reads the header and spreads it.
//
// Everyone refused at the same instant received the same value, and waiting
// exactly that long reconstitutes the convoy the refusal was meant to break.
// Between half and one and a half times the announced delay.
func (c *Client) retryAfter(resp *http.Response) time.Duration {
	announced := defaultRetryAfter
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds > 0 {
			announced = time.Duration(seconds) * time.Second
		}
	}
	spread := 0.5 + c.random()
	return time.Duration(float64(announced) * spread)
}

// defaultRetryAfter is what a refusal with no header is worth waiting. A
// saturated pool frees a slot in seconds.
const defaultRetryAfter = 5 * time.Second
