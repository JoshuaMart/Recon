//go:build integration

package runs_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/api"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// fakeJobs is the platform: it records what it was asked to start and says
// what the test sets about each job.
type fakeJobs struct {
	mu      sync.Mutex
	started [][]string
	states  map[string]runs.JobState
	stopped []string
}

func (f *fakeJobs) Start(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, args)
	return "job-" + string(rune('a'+len(f.started)-1)), nil
}

func (f *fakeJobs) State(_ context.Context, id string) (runs.JobState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id], nil
}

func (f *fakeJobs) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeJobs) last(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.started)
		f.mu.Unlock()
		if n > 0 {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.started[n-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no job started")
	return nil
}

func header(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// auth is the value of an "Authorization: ..." header argument.
func auth(args []string, flag string) string {
	return strings.TrimPrefix(header(args, flag), "Authorization: ")
}

type jobWorld struct {
	clock  *manualClock
	f      *fixture
	jobs   *fakeJobs
	srv    *httptest.Server
	tokens *runs.Tokens
}

func jobSetup(t *testing.T) *jobWorld {
	t.Helper()
	f := setup(t, readFixture(t))
	jobs := &fakeJobs{states: map[string]runs.JobState{}}
	tokens := runs.NewTokens("0123456789abcdef0123456789abcdef")
	f.sched.Jobs, f.sched.StartJobs, f.sched.JobSlots, f.sched.Tokens = jobs, true, 2, tokens
	f.sched.Grace, f.sched.ReportTimeout = 30*time.Minute, 5*time.Minute
	c := &manualClock{t: time.Now().UTC()}
	f.sched.Now = c.now
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.Handler(api.Deps{Log: log, DB: f.st, Store: f.st, RunJobs: f.sched, RunTokens: tokens}))
	t.Cleanup(srv.Close)
	f.sched.CallbackURL = srv.URL
	return &jobWorld{clock: c, f: f, jobs: jobs, srv: srv, tokens: tokens}
}

func (w *jobWorld) do(t *testing.T, method, url, auth string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	// Bounded: a copy that would wait behind a lock fails the test rather
	// than hanging it.
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAReconRunsAsAJobAndPostsItsReport(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, err := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	args := w.jobs.last(t)
	if header(args, "-d") != "example.com" || header(args, "--webhook-url") != w.srv.URL+"/runs/"+r.ID.String()+"/report" {
		t.Fatalf("args = %v", args)
	}
	if len(w.f.runner.requests) != 0 {
		t.Fatal("a recon went to serve while jobs are configured")
	}
	report := auth(args, "--webhook-header")

	// Nothing without the run's own report token.
	if code, _ := w.do(t, "POST", w.srv.URL+"/runs/"+r.ID.String()+"/report", "Bearer nope", readFixture(t)); code != 401 {
		t.Fatalf("a bad token = %d", code)
	}
	if code, _ := w.do(t, "POST", w.srv.URL+"/runs/"+r.ID.String()+"/report", "Bearer "+w.tokens.Mint(runs.PurposeTargets, r.ID, time.Now().Add(time.Hour)), readFixture(t)); code != 401 {
		t.Fatalf("a targets token posting a report = %d", code)
	}
	code, body := w.do(t, "POST", w.srv.URL+"/runs/"+r.ID.String()+"/report", report, readFixture(t))
	if code != 200 || !strings.Contains(body, `"services_created":3`) {
		t.Fatalf("report = %d %s", code, body)
	}
	got, _ := w.f.st.Q.GetRun(ctx, r.ID)
	if got.State != "succeeded" || got.Runner != "scaleway" || got.ExternalID == nil || *got.ExternalID != "job-a" {
		t.Fatalf("run = %s on %s, job %v", got.State, got.Runner, got.ExternalID)
	}
	// FastRecon retries a delivery: the second copy changes nothing.
	if code, _ := w.do(t, "POST", w.srv.URL+"/runs/"+r.ID.String()+"/report", report, readFixture(t)); code != 409 {
		t.Fatalf("a second copy = %d, want 409", code)
	}
	if ns, _ := w.f.st.Q.RecentNotifications(ctx); len(ns) != 1 || ns[0].Kind != "recon_finished" {
		t.Fatalf("notifications = %+v", ns)
	}
}

func TestAVerificationJobFetchesItsTargets(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, err := w.f.st.Q.EnqueueVerification(ctx, sqlcgen.EnqueueVerificationParams{
		ProgramID: w.f.program.ID, Stage: "resolve", Targets: []string{"a.example.com", "b.example.com"}, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	args := w.jobs.last(t)
	if header(args, "--targets-url") != w.srv.URL+"/runs/"+r.ID.String()+"/targets" {
		t.Fatalf("args = %v", args)
	}
	code, body := w.do(t, "GET", header(args, "--targets-url"), auth(args, "--targets-header"), nil)
	if code != 200 || body != "a.example.com\nb.example.com\n" {
		t.Fatalf("targets = %d %q", code, body)
	}
	if code, _ := w.do(t, "GET", header(args, "--targets-url"), auth(args, "--webhook-header"), nil); code != 401 {
		t.Fatalf("the report token fetching targets = %d", code)
	}
}

func TestAJobThePlatformCallsFailedClosesItsRun(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, _ := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	w.jobs.last(t)
	waitExternal(t, w, r.ID.String())
	code := 3
	w.jobs.mu.Lock()
	w.jobs.states["job-a"] = runs.JobState{Ended: true, Failed: true, Reason: "failed, exit code 3", ExitCode: &code}
	w.jobs.mu.Unlock()
	// The jobs are read once a minute.
	w.clock.set(w.clock.now().Add(2 * time.Minute))
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := w.f.st.Q.GetRun(ctx, r.ID)
	if got.State != "failed" || got.Error == nil || !strings.Contains(*got.Error, "exit 3: delivery failed") {
		t.Fatalf("run = %s %v", got.State, got.Error)
	}
	if ns, _ := w.f.st.Q.RecentNotifications(ctx); len(ns) != 1 || !strings.Contains(ns[0].Title, "failed") {
		t.Fatalf("notifications = %+v", ns)
	}
}

// A job keeps running across a restart of Recon: it is still waited for,
// where a serve call cut by the restart is failed.
func TestARestartKeepsWaitingForJobs(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, _ := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	w.jobs.last(t)
	waitExternal(t, w, r.ID.String())
	if err := w.f.sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.f.st.Q.GetRun(ctx, r.ID); got.State != "running" {
		t.Fatalf("a job's run after a restart = %s, want still running", got.State)
	}
}

func TestATimedOutJobIsStopped(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, _ := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	w.jobs.last(t)
	waitExternal(t, w, r.ID.String())
	if _, err := w.f.st.Pool.Exec(ctx, `UPDATE run SET deadline = now() - interval '1 minute' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(w.jobs.stopped, "job-a") {
		t.Fatalf("stopped = %v, want job-a", w.jobs.stopped)
	}
}

func waitExternal(t *testing.T, w *jobWorld, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ext *string
		_ = w.f.st.Pool.QueryRow(context.Background(), `SELECT external_id FROM run WHERE id = $1`, id).Scan(&ext)
		if ext != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the job's id was never recorded")
}

// CT candidates stay on serve: a job's start would eat their minutes.
func TestCandidatesStayOnServe(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	if _, err := w.f.st.Q.EnqueueCandidate(ctx, sqlcgen.EnqueueCandidateParams{
		ProgramID: w.f.program.ID, Stage: "resolve", Targets: []string{"new.example.com"}, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, w.f)
	w.jobs.mu.Lock()
	started := len(w.jobs.started)
	w.jobs.mu.Unlock()
	w.f.runner.mu.Lock()
	defer w.f.runner.mu.Unlock()
	if started != 0 || len(w.f.runner.requests) != 1 || len(w.f.runner.requests[0].Targets) != 1 {
		t.Fatalf("jobs started %d, serve requests %+v: want the candidate on serve", started, w.f.runner.requests)
	}
}

// Back on serve, a job started before is still followed to its end, and new
// runs go to serve.
func TestASwitchBackToServeStillFollowsTheJobs(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, _ := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	w.jobs.last(t)
	waitExternal(t, w, r.ID.String())

	w.f.sched.StartJobs = false
	w.jobs.mu.Lock()
	w.jobs.states["job-a"] = runs.JobState{Ended: true, Failed: true, Reason: "failed"}
	w.jobs.mu.Unlock()
	w.clock.set(w.clock.now().Add(2 * time.Minute))
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.f.st.Q.GetRun(ctx, r.ID); got.State != "failed" {
		t.Fatalf("the job started before the switch = %s, want followed to failed", got.State)
	}
	if _, _, _, err := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, w.f)
	w.jobs.mu.Lock()
	started := len(w.jobs.started)
	w.jobs.mu.Unlock()
	if started != 1 || len(w.f.runner.requests) != 1 {
		t.Fatalf("jobs %d, serve %d: want the new recon on serve", started, len(w.f.runner.requests))
	}
}

// A copy posted while the first is being ingested is turned away at once,
// not ingested a second time.
func TestACopyDuringIngestionIsTurnedAway(t *testing.T) {
	ctx := context.Background()
	w := jobSetup(t)
	r, _, _, _ := runs.EnqueueRecon(ctx, w.f.st.Q, w.f.wildcard, "user", time.Now().UTC())
	if err := w.f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	args := w.jobs.last(t)
	tx, err := w.f.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The first copy's transaction, holding the run.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM run WHERE id = $1 FOR UPDATE`, r.ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, _ := w.do(t, "POST", header(args, "--webhook-url"), auth(args, "--webhook-header"), readFixture(t))
	if code != 409 || time.Since(start) > 5*time.Second {
		t.Fatalf("a copy during ingestion = %d after %s, want 409 at once", code, time.Since(start))
	}
	_ = tx.Rollback(ctx)
	if code, _ := w.do(t, "POST", header(args, "--webhook-url"), auth(args, "--webhook-header"), readFixture(t)); code != 200 {
		t.Fatalf("the report once the run is free = %d", code)
	}
}
