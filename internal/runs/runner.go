// Package runs queues FastRecon runs, sends them to a runner, and hands the
// report to ingestion.
package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Request is what a run asks FastRecon to do: a recon carries a domain, a
// verification a target list.
type Request struct {
	Domain  string   `json:"domain,omitempty"`
	Targets []string `json:"targets,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	Stages  string   `json:"stages"`
	Ports   string   `json:"ports,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

// Errors a runner returns for the scheduler to act on.
var (
	// ErrBusy means the runner had no free instance: the run goes back to the queue.
	ErrBusy = errors.New("runner busy")
	// ErrRejected means the runner refused the request itself: retrying fails the same way.
	ErrRejected = errors.New("run rejected")
)

// Runner executes one run and returns its report.
type Runner interface {
	Run(ctx context.Context, req Request) ([]byte, error)
}

// Serve is a `fastrecon serve` instance: the report is the response.
type Serve struct {
	url   string
	token string
	http  *http.Client
}

// NewServe builds a runner for the instance at url. timeout is the run's own
// budget; the HTTP call waits a margin longer for the report to arrive.
func NewServe(url, token string, timeout time.Duration) *Serve {
	return &Serve{url: strings.TrimSuffix(url, "/"), token: token, http: &http.Client{Timeout: timeout + 5*time.Minute}}
}

// maxReport bounds a report: a six-figure host count is tens of megabytes.
const maxReport = 512 << 20

// Run posts the request and reads the report.
func (s *Serve) Run(ctx context.Context, req Request) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+s.token)
	hr.Header.Set("Content-Type", "application/json")
	res, err := s.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("fastrecon: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxReport))
	if err != nil {
		return nil, fmt.Errorf("fastrecon: reading the report: %w", err)
	}
	switch res.StatusCode {
	case http.StatusOK:
		return raw, nil
	case http.StatusTooManyRequests:
		return nil, ErrBusy
	case http.StatusBadRequest:
		return nil, fmt.Errorf("%w: %s", ErrRejected, snippet(raw))
	}
	return nil, fmt.Errorf("fastrecon: %s: %s", res.Status, snippet(raw))
}

func snippet(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}
