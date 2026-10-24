package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Jobs starts and follows remote runs.
type Jobs interface {
	// Start runs the definition with these arguments and returns the
	// platform's id for the execution.
	Start(ctx context.Context, args []string) (string, error)
	// State reads what became of an execution.
	State(ctx context.Context, id string) (JobState, error)
	// Stop ends an execution; one already ended is not an error.
	Stop(ctx context.Context, id string) error
}

// JobState is what the platform says of an execution. An unknown state is
// neither running nor failed: the run is left to its deadline.
type JobState struct {
	Running bool
	Ended   bool
	// Failed is an ending that delivered nothing the platform knows of:
	// failed, canceled, interrupted. A zero exit is not one: its report is
	// on its way or in.
	Failed bool
	// Reason is the platform's word for the ending, never the log tail, which
	// is the scanner's report.
	Reason   string
	ExitCode *int
}

// Scaleway starts a Serverless Job definition per run. Two measured facts
// shape it: an update replaces the definition's whole environment (the source
// keys would go without an error), so it only ever starts; and a start's
// arguments replace the definition's while its environment merges, so the
// whole invocation travels as arguments and a -d left on the definition can
// never pin a run.
type Scaleway struct {
	Endpoint string
	Region   string
	JobID    string
	Secret   string
	HTTP     *http.Client
}

const scalewayAPI = "/serverless-jobs/v1alpha2/regions/"

// defaultClient is shared, so connections to the API are reused.
var defaultClient = &http.Client{Timeout: 30 * time.Second}

func (s *Scaleway) url(path string) string {
	return strings.TrimRight(s.Endpoint, "/") + scalewayAPI + url.PathEscape(s.Region) + path
}

// call makes one authenticated request. An error body is quoted whole: a
// quota refusal and a wrong definition id share a status and call for
// opposite actions.
func (s *Scaleway) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url(path), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", s.Secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := s.HTTP
	if client == nil {
		client = defaultClient
	}
	res, err := client.Do(req) //nolint:gosec // the Scaleway API from the config
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("%s %s: %w", method, path, ue.Err)
		}
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return payload, &APIError{Status: res.StatusCode, Body: string(bytes.TrimSpace(payload))}
	}
	return payload, nil
}

// APIError is a refusal from the platform, with what it said.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("scaleway answered %d: %s", e.Status, e.Body) }

// Start runs the definition with this run's arguments.
func (s *Scaleway) Start(ctx context.Context, args []string) (string, error) {
	payload, err := s.call(ctx, http.MethodPost, "/job-definitions/"+url.PathEscape(s.JobID)+"/start",
		map[string]any{"args": args})
	if err != nil {
		return "", fmt.Errorf("starting the job: %w", err)
	}
	var out struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"job_runs"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("reading the start answer: %w", err)
	}
	if len(out.Runs) == 0 || out.Runs[0].ID == "" {
		return "", errors.New("the platform accepted the start and named no job run")
	}
	return out.Runs[0].ID, nil
}

var (
	jobEnded  = map[string]bool{"succeeded": true, "failed": true, "canceled": true, "interrupted": true}
	jobFailed = map[string]bool{"failed": true, "canceled": true, "interrupted": true}
)

// State reads one job run.
func (s *Scaleway) State(ctx context.Context, id string) (JobState, error) {
	payload, err := s.call(ctx, http.MethodGet, "/job-runs/"+url.PathEscape(id), nil)
	if err != nil {
		return JobState{}, fmt.Errorf("reading the job run: %w", err)
	}
	var run struct {
		State    string `json:"state"`
		Reason   string `json:"reason"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := json.Unmarshal(payload, &run); err != nil {
		return JobState{}, fmt.Errorf("reading the job run: %w", err)
	}
	st := JobState{Running: run.State == "running", Ended: jobEnded[run.State], Failed: jobFailed[run.State], ExitCode: run.ExitCode}
	if st.Failed {
		st.Reason = run.State
		if run.Reason != "" {
			st.Reason += ", " + run.Reason
		}
	}
	return st, nil
}

// Stop ends a job run. The platform refuses to stop one that ended, which is
// the outcome wanted, so a refusal is checked against the run's state.
func (s *Scaleway) Stop(ctx context.Context, id string) error {
	_, err := s.call(ctx, http.MethodPost, "/job-runs/"+url.PathEscape(id)+"/stop", map[string]any{})
	if err == nil {
		return nil
	}
	if st, stateErr := s.State(ctx, id); stateErr == nil && st.Ended {
		return nil
	}
	return fmt.Errorf("stopping the job run: %w", err)
}

// Check reads the definition once, at startup: it must exist, retry nothing
// and outlive the runs Recon gives it. A misconfiguration is then a log line
// at boot, not a first run that dies.
func (s *Scaleway) Check(ctx context.Context, runTimeout time.Duration) error {
	payload, err := s.call(ctx, http.MethodGet, "/job-definitions/"+url.PathEscape(s.JobID), nil)
	if err != nil {
		return fmt.Errorf("reading the job definition: %w", err)
	}
	var def struct {
		Timeout string `json:"job_timeout"`
		Retry   struct {
			MaxRetries int `json:"max_retries"`
		} `json:"retry_policy"`
	}
	if err := json.Unmarshal(payload, &def); err != nil {
		return fmt.Errorf("reading the job definition: %w", err)
	}
	var problems []string
	if def.Retry.MaxRetries != 0 {
		// The platform retries on any non-zero exit and cannot tell FastRecon's
		// exit codes apart: a retry is a duplicate run and a duplicate report.
		problems = append(problems, fmt.Sprintf("it retries %d times, set retry-policy.max-retries=0", def.Retry.MaxRetries))
	}
	if t, err := time.ParseDuration(def.Timeout); err != nil {
		problems = append(problems, fmt.Sprintf("its timeout %q does not read as a duration", def.Timeout))
	} else if t <= runTimeout+5*time.Minute {
		// The job must outlive FastRecon's own budget, image pull and report
		// delivery included, or it is killed before delivering anything.
		problems = append(problems, fmt.Sprintf("its timeout %s does not outlive the run's %s and a margin", t, runTimeout))
	}
	if len(problems) > 0 {
		return fmt.Errorf("job definition %s: %s", s.JobID, strings.Join(problems, "; "))
	}
	return nil
}
