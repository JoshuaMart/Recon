package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JoshuaMart/recon/internal/runs"
)

// Scaleway starts a serverless job definition. Two measured facts shape the call: arguments
// replace while the environment merges (the definition keeps the source API keys, and the
// control plane sends only what it owns), so a flag on the definition beats a variable sent
// here ("-d hackerone.com" there would make every run scan that, silently). The whole
// invocation therefore travels as arguments.
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
		// The detail is quoted, not summarised: a quota refusal and a wrong definition id
		// share a status code and call for opposite actions.
		return "", fmt.Errorf("start run: %s: %s", resp.Status, bytes.TrimSpace(payload))
	}

	var decoded startResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode start response: %w", err)
	}
	if len(decoded.Runs) == 0 {
		// Accepted with nothing named. The run row exists and the sweeper owns it, so this
		// is reported, not treated as a failure to start.
		s.log.WarnContext(ctx, "the platform started a run and named none", "run", def.RunID)
		return "", nil
	}
	return decoded.Runs[0].ID, nil
}

// jobRun is the part of a job run this reads, transcribed from a real failed run:
// state, the platform's reason and exit code. error_message is left out because it
// is the truncated tail of the container's output, which is the scanner's report.
type jobRun struct {
	State    string `json:"state"`
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exit_code"`
}

// ended is every state the platform does not leave again.
var ended = map[string]bool{
	"succeeded": true, "failed": true, "canceled": true, "interrupted": true,
}

// failedStates are the endings that delivered nothing the platform knows of.
// succeeded is not one: a zero exit has delivered its report, so a run still open
// after it is a report being written.
var failedStates = map[string]bool{"failed": true, "canceled": true, "interrupted": true}

// Status reads what became of an execution.
func (s *Scaleway) Status(ctx context.Context, externalID string) (runs.Execution, error) {
	run, err := s.read(ctx, externalID)
	if err != nil {
		return runs.Execution{}, err
	}
	execution := runs.Execution{
		Running:  run.State == "running",
		Failed:   failedStates[run.State],
		ExitCode: run.ExitCode,
	}
	if execution.Failed {
		execution.Reason = run.State
		if run.Reason != "" {
			execution.Reason += ", " + run.Reason
		}
	}
	return execution, nil
}

// Stop asks the platform to end an execution. It refuses to stop what already
// ended, which is the outcome wanted, so a refused stop is checked against the
// execution's state before being reported.
func (s *Scaleway) Stop(ctx context.Context, externalID string) error {
	status, payload, err := s.call(ctx, http.MethodPost, s.jobRunURL(externalID)+"/stop")
	if err != nil {
		return fmt.Errorf("stop run: %w", err)
	}
	if status < 300 {
		return nil
	}
	if run, readErr := s.read(ctx, externalID); readErr == nil && ended[run.State] {
		return nil
	}
	return fmt.Errorf("stop run: %d: %s", status, payload)
}

func (s *Scaleway) read(ctx context.Context, externalID string) (jobRun, error) {
	status, payload, err := s.call(ctx, http.MethodGet, s.jobRunURL(externalID))
	if err != nil {
		return jobRun{}, fmt.Errorf("read run: %w", err)
	}
	if status >= 300 {
		return jobRun{}, fmt.Errorf("read run: %d: %s", status, payload)
	}
	var run jobRun
	if err := json.Unmarshal(payload, &run); err != nil {
		return jobRun{}, fmt.Errorf("decode run: %w", err)
	}
	return run, nil
}

func (s *Scaleway) jobRunURL(externalID string) string {
	return fmt.Sprintf("%s/serverless-jobs/v1alpha2/regions/%s/job-runs/%s",
		s.endpoint, s.region, url.PathEscape(externalID))
}

// call makes one authenticated request and returns the status and body. Error
// bodies are quoted by the caller, as in Start.
func (s *Scaleway) call(ctx context.Context, method, target string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Auth-Token", s.secret)
	if method == http.MethodPost {
		// The stop takes an empty object; an empty body is refused as not being one.
		req.Body = io.NopCloser(strings.NewReader("{}"))
		req.ContentLength = 2
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, bytes.TrimSpace(payload), nil
}
