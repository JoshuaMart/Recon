package runner_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/runner"
	"github.com/JoshuaMart/recon/internal/runs"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The response envelope is transcribed from what the API actually returns, and
// a body of another shape unmarshals cleanly into nothing. Without this test
// that failure is a warning and an execution whose logs nobody can find, which
// is the one thing the external id exists for.
func TestTheStartResponseIsReadTheWayTheAPIWritesIt(t *testing.T) {
	t.Parallel()

	var seen struct {
		path   string
		token  string
		body   map[string]any
		method string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.token, seen.method = r.URL.Path, r.Header.Get("X-Auth-Token"), r.Method
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		// Measured against the real endpoint: a list, not a bare object.
		_, _ = io.WriteString(w, `{"job_runs":[{"id":"run-42","state":"validated"}]}`)
	}))
	defer server.Close()

	job := uuid.NewString()
	start := runner.NewScaleway(server.URL, "fr-par", job, "secret", 5*time.Second, quiet())

	external, err := start.Start(context.Background(), &runs.Definition{
		RunID: uuid.New(),
		Kind:  runs.KindDiscovery,
		Args:  []string{"--stages", "full", "-d", "acme.test"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if external != "run-42" {
		t.Fatalf("the platform's name for the run is %q", external)
	}

	if seen.method != http.MethodPost {
		t.Errorf("the start is a %s", seen.method)
	}
	if want := "/serverless-jobs/v1alpha2/regions/fr-par/job-definitions/" + job + "/start"; seen.path != want {
		t.Errorf("the start went to %q, want %q", seen.path, want)
	}
	if seen.token != "secret" {
		t.Errorf("the credential travelled as %q", seen.token)
	}
	// Arguments replace the definition's wholesale, which is the reason the
	// whole invocation travels as arguments in the first place.
	args, _ := seen.body["args"].([]any)
	if len(args) != 4 || args[0] != "--stages" {
		t.Errorf("the invocation sent was %v", args)
	}
}

// A refusal has to be a refusal. A quota and a wrong definition id read the
// same at the status code, so the detail is quoted rather than summarised.
func TestARefusedStartIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"quota exceeded"}`)
	}))
	defer server.Close()

	start := runner.NewScaleway(server.URL, "fr-par", uuid.NewString(), "secret", 5*time.Second, quiet())
	_, err := start.Start(context.Background(), &runs.Definition{RunID: uuid.New()})
	if err == nil {
		t.Fatal("a refused start came back as a success")
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("the refusal reads %v, and a quota and a wrong id look alike without the detail", err)
	}
}

// An invocation is written down in logs and in a run record. Both credentials
// it carries are bearer tokens for the life of the run they name.
func TestAnInvocationIsSafeToWriteDown(t *testing.T) {
	t.Parallel()

	args := []string{
		"--webhook-url", "https://recon.example/reports",
		"--webhook-header", "Authorization: Bearer v1.report.secret",
		"--targets-header", "Authorization: Bearer v1.targets.secret",
		"--stages", "full",
	}
	redacted := strings.Join(runs.Redacted(args), " ")

	if strings.Contains(redacted, "secret") {
		t.Fatalf("a credential survived redaction: %s", redacted)
	}
	for _, kept := range []string{"--stages", "full", "https://recon.example/reports"} {
		if !strings.Contains(redacted, kept) {
			t.Errorf("%q was redacted, and it is what makes the line readable", kept)
		}
	}
	// The original is untouched, or the run that was about to start loses its
	// credentials to the log line describing it.
	if args[3] != "Authorization: Bearer v1.report.secret" {
		t.Fatal("redaction rewrote the invocation it was asked to describe")
	}
}

// The body is the one the platform returned for a run whose report could not
// be delivered, minus the fields this does not read. error_message is kept in
// it on purpose: it is the tail of what the scanner printed, which is its
// report, and the reason must not carry any of it.
func TestAFailedExecutionIsReadWithItsReasonAndExitCode(t *testing.T) {
	t.Parallel()

	var path, token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.Header.Get("X-Auth-Token")
		_, _ = io.WriteString(w, `{"id":"run-42","state":"failed","reason":"exited_with_error",`+
			`"exit_code":3,"error_message":"[TRUNCATED MESSAGE]: \"host\":\"www.acme.test\""}`)
	}))
	defer server.Close()

	platform := runner.NewScaleway(server.URL, "fr-par", uuid.NewString(), "secret", 5*time.Second, quiet())
	execution, err := platform.Status(context.Background(), "run-42")
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if path != "/serverless-jobs/v1alpha2/regions/fr-par/job-runs/run-42" {
		t.Errorf("the status was read at %s", path)
	}
	if token != "secret" {
		t.Errorf("the status was read with %q", token)
	}
	if !execution.Failed || execution.Running {
		t.Fatalf("a failed execution read as %+v", execution)
	}
	if execution.ExitCode == nil || *execution.ExitCode != 3 {
		t.Errorf("the exit code is %v", execution.ExitCode)
	}
	if execution.Reason != "failed, exited_with_error" {
		t.Errorf("the reason is %q", execution.Reason)
	}
	if strings.Contains(execution.Reason, "acme") {
		t.Error("the reason carries the scanner's output")
	}
}

// Only the endings that delivered nothing the platform knows of fail a run. A
// succeeded execution has delivered its report, and a state this does not know
// leaves the run to its deadline.
func TestOnlyAFailingEndingFailsARun(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ running, failed bool }{
		"queued":      {},
		"running":     {running: true},
		"succeeded":   {},
		"failed":      {failed: true},
		"canceled":    {failed: true},
		"interrupted": {failed: true},
		"a_new_state": {},
	}
	for state, want := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		}))
		platform := runner.NewScaleway(server.URL, "fr-par", "job", "secret", 5*time.Second, quiet())
		execution, err := platform.Status(context.Background(), "run-42")
		server.Close()
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if execution.Running != want.running || execution.Failed != want.failed {
			t.Errorf("%s read as running=%v failed=%v", state, execution.Running, execution.Failed)
		}
	}
}

// The platform refuses to stop an execution that already ended, and that
// refusal is the outcome a cancel asked for. A refusal on a running execution
// is a real one.
func TestAStopRefusedOnAnEndedExecutionIsNotAnError(t *testing.T) {
	t.Parallel()

	for state, wantErr := range map[string]bool{"failed": false, "succeeded": false, "running": true} {
		var stopped bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/job-runs/run-42/stop") {
				stopped = true
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"message":"job run cannot be stopped"}`)
				return
			}
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		}))
		platform := runner.NewScaleway(server.URL, "fr-par", "job", "secret", 5*time.Second, quiet())
		err := platform.Stop(context.Background(), "run-42")
		server.Close()

		if !stopped {
			t.Fatalf("%s: the stop was never asked", state)
		}
		if (err != nil) != wantErr {
			t.Errorf("%s: a refused stop returned %v", state, err)
		}
	}
}
