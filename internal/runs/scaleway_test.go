package runs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

func fakeScaleway(t *testing.T, h http.HandlerFunc) *Scaleway {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Scaleway{Endpoint: srv.URL, Region: "fr-par", JobID: "def-1", Secret: "scw-secret"}
}

func TestStartSendsTheInvocationAsArguments(t *testing.T) {
	var got struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"environment_variables"`
	}
	s := fakeScaleway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/serverless-jobs/v1alpha2/regions/fr-par/job-definitions/def-1/start" ||
			r.Header.Get("X-Auth-Token") != "scw-secret" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"job_runs":[{"id":"run-9","state":"queued"}]}`))
	})
	id, err := s.Start(context.Background(), []string{"--stages", "full", "-d", "example.com"})
	if err != nil || id != "run-9" {
		t.Fatalf("start = %q %v", id, err)
	}
	// Arguments only: the environment merges with the definition's, and
	// nothing Recon sends there may shadow the source keys.
	if !slices.Equal(got.Args, []string{"--stages", "full", "-d", "example.com"}) || got.Env != nil {
		t.Fatalf("body = %+v", got)
	}
}

func TestARefusalIsQuotedWhole(t *testing.T) {
	s := fakeScaleway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"quota exceeded","type":"quotas_exceeded"}`))
	})
	_, err := s.Start(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "quotas_exceeded") || strings.Contains(err.Error(), "scw-secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestStateReadsTheEnding(t *testing.T) {
	answers := map[string]string{
		"/job-runs/a": `{"state":"running"}`,
		"/job-runs/b": `{"state":"failed","reason":"exit code 3","exit_code":3}`,
		"/job-runs/c": `{"state":"succeeded","exit_code":0}`,
		"/job-runs/d": `{"state":"something new"}`,
	}
	s := fakeScaleway(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answers[strings.TrimPrefix(r.URL.Path, "/serverless-jobs/v1alpha2/regions/fr-par")]))
	})
	for id, want := range map[string]JobState{
		"a": {Running: true},
		"b": {Ended: true, Failed: true, Reason: "failed, exit code 3"},
		"c": {Ended: true},
		"d": {},
	} {
		got, err := s.State(context.Background(), id)
		if err != nil || got.Running != want.Running || got.Ended != want.Ended || got.Failed != want.Failed || got.Reason != want.Reason {
			t.Errorf("%s = %+v %v, want %+v", id, got, err, want)
		}
	}
}

func TestStoppingAnEndedJobIsNotAnError(t *testing.T) {
	s := fakeScaleway(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		_, _ = w.Write([]byte(`{"state":"succeeded"}`))
	})
	if err := s.Stop(context.Background(), "x"); err != nil {
		t.Fatalf("stop of an ended job = %v", err)
	}
}

func TestCheckRefusesADefinitionThatRetriesOrDiesFirst(t *testing.T) {
	def := ""
	s := fakeScaleway(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(def)) })
	def = `{"job_timeout":"18000.000000000s","retry_policy":{"max_retries":0}}`
	if err := s.Check(context.Background(), 30*time.Minute); err != nil {
		t.Fatalf("a usable definition refused: %v", err)
	}
	def = `{"job_timeout":"1800s","retry_policy":{"max_retries":2}}`
	err := s.Check(context.Background(), 30*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "retries 2") || !strings.Contains(err.Error(), "does not outlive") {
		t.Fatalf("err = %v", err)
	}
}

func TestJobArgsCarryTokensInHeadersOnly(t *testing.T) {
	tokens := NewTokens("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(40 * time.Minute)
	s := &Scheduler{Tokens: tokens, CallbackURL: "https://recon.test/", Grace: 30 * time.Minute, ReportTimeout: 5 * time.Minute, Now: func() time.Time { return now }}
	r := sqlcgen.Run{ID: uuid.New(), Deadline: &deadline}

	recon := s.JobArgs(r, Request{Domain: "example.com", Exclude: []string{"admin.example.com"}, Stages: "full", Ports: "80,443", Timeout: "30m"})
	joined := strings.Join(recon, " ")
	for _, want := range []string{"-d example.com", "--exclude admin.example.com", "--timeout 30m", "--webhook-url https://recon.test/runs/" + r.ID.String() + "/report", "--webhook-timeout 5m0s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("recon args lack %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--targets-url") {
		t.Error("a recon was given a target list")
	}
	header := recon[slices.Index(recon, "--webhook-header")+1]
	tok, _ := strings.CutPrefix(header, "Authorization: Bearer ")
	if tokens.Check(tok, PurposeReport, r.ID, deadline.Add(29*time.Minute)) != nil {
		t.Error("the report token does not last the grace past the deadline")
	}
	if tokens.Check(tok, PurposeTargets, r.ID, now) == nil {
		t.Error("the report token fetches targets")
	}

	verify := strings.Join(s.JobArgs(r, Request{Targets: []string{"a.example.com"}, Stages: "resolve", Ports: "80", Timeout: "30m"}), " ")
	if !strings.Contains(verify, "--targets-url https://recon.test/runs/"+r.ID.String()+"/targets --targets-header Authorization: Bearer ") || strings.Contains(verify, " -d ") {
		t.Errorf("verification args = %s", verify)
	}
	for _, a := range redact(s.JobArgs(r, Request{Targets: []string{"x"}})) {
		if strings.Contains(a, "Bearer") {
			t.Fatalf("redact kept a token: %s", a)
		}
	}
}
