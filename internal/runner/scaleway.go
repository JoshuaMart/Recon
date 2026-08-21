package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/JoshuaMart/recon/internal/runs"
)

// Scaleway starts a serverless job definition.
//
// Two facts about that API decide the shape of what is sent, and both were
// measured against it rather than read into it:
//
//   - **Arguments replace, the environment merges.** A start call swaps the
//     definition's arguments wholesale and adds its variables to the ones the
//     definition already holds. The merge is what lets the definition keep the
//     source API keys while the control plane sends only what it owns.
//   - **A flag on the definition therefore beats a variable sent here.** A
//     definition carrying "-d hackerone.com" would make every run scan that,
//     whatever the environment said, and nothing would look wrong. So the whole
//     invocation travels as arguments.
type Scaleway struct {
	endpoint string
	region   string
	job      string
	secret   string
	http     *http.Client
	log      *slog.Logger
}

// NewScaleway builds the starter.
func NewScaleway(endpoint, region, job, secret string, timeout time.Duration, log *slog.Logger) *Scaleway {
	return &Scaleway{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		region:   region,
		job:      job,
		secret:   secret,
		http:     &http.Client{Timeout: timeout},
		log:      log,
	}
}

// Name is what a log line calls this platform.
func (s *Scaleway) Name() string { return "scaleway" }

type startRequest struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"environment_variables,omitempty"`
}

type startResponse struct {
	Runs []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"job_runs"`
}

// Start runs the definition with this run's overrides.
func (s *Scaleway) Start(ctx context.Context, def *runs.Definition) (string, error) {
	body, err := json.Marshal(startRequest{Args: def.Args, Env: def.Env})
	if err != nil {
		return "", fmt.Errorf("encode start request: %w", err)
	}

	url := fmt.Sprintf("%s/serverless-jobs/v1alpha2/regions/%s/job-definitions/%s/start",
		s.endpoint, s.region, s.job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build start request: %w", err)
	}
	req.Header.Set("X-Auth-Token", s.secret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("start run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read start response: %w", err)
	}
	if resp.StatusCode >= 300 {
		// The detail is quoted rather than summarised. A quota refusal and a
		// wrong definition id read the same way at the status code, and they
		// call for opposite actions.
		return "", fmt.Errorf("start run: %s: %s", resp.Status, bytes.TrimSpace(payload))
	}

	var decoded startResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode start response: %w", err)
	}
	if len(decoded.Runs) == 0 {
		// Accepted with nothing named. The run row still exists and the
		// deadline sweeper still owns it, so this is reported rather than
		// treated as a failure to start.
		s.log.WarnContext(ctx, "the platform started a run and named none", "run", def.RunID)
		return "", nil
	}
	return decoded.Runs[0].ID, nil
}
