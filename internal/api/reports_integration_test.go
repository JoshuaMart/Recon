//go:build integration

// The half of milestone 1 that only exists once a report arrives over HTTP.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/api"
	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store"
)

const signingKey = "a-signing-key-long-enough-to-be-one"

type harness struct {
	pool    *pgxpool.Pool
	server  *httptest.Server
	signer  *auth.Signer
	sched   *runs.Scheduler
	clock   *clock
	org     uuid.UUID
	program uuid.UUID
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("recon"),
		tcpostgres.WithUsername("asm_owner"),
		tcpostgres.WithPassword("owner-password-for-a-container"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	migrator, err := store.NewMigrator(url, quiet)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := migrator.Run(ctx, store.Up); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = migrator.Close()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	c := &clock{now: time.Now()}
	cfg := config.Defaults().Verification
	cfg.PublicURL = "https://recon.example"

	h := &harness{
		pool:    pool,
		signer:  signer,
		org:     uuid.New(),
		program: uuid.New(),
		clock:   c,
		sched:   runs.New(signer, cfg, quiet, runs.WithClock(c.Now)),
	}
	ingestor := ingest.New(nil, lifecycle.DefaultCadence(), quiet)

	// The whole route set rather than one handler. What the tests below check
	// about a refusal is which of them answers, and a mux built per test would
	// let a route be reachable in a test and unrouted in the binary.
	mux := http.NewServeMux()
	mux.Handle("POST /reports", api.NewReports(pool, signer, ingestor, quiet))
	mux.Handle("GET /runs/{run}/targets", api.NewTargets(pool, signer, quiet))
	guard := api.NewGuard(pool, quiet)
	programs := api.NewPrograms(pool, h.sched, ingestor, time.Minute, quiet)
	mux.Handle("POST /programs/{program}/runs", guard.Require(auth.ActionManageJobs, programs.StartRun))
	mux.Handle("POST /programs/{program}/assets", guard.Require(auth.ActionManageScope, programs.EnterAssets))
	renders := api.NewRenders(pool, 72*time.Hour, quiet)
	mux.Handle("POST /assets/{asset}/render", guard.Require(auth.ActionManageJobs, renders.Request))
	mux.Handle("POST /renders/replan", guard.Require(auth.ActionManageJobs, renders.Replan))

	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, h.org)
	h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', now() - interval '1 day')`,
		h.program, h.org)
	h.exec(t, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern)
	           VALUES ($1, $2, $3, 'include', 'apex', 'target.test')`,
		uuid.New(), h.org, h.program)
	return h
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// run inserts an execution and returns the credential it would have been
// started with.
func (h *harness) run(t *testing.T, kind string) (uuid.UUID, string) {
	t.Helper()

	id := uuid.New()
	h.exec(t, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
	           VALUES ($1, $2, $3, $4, 'full', 'pending', now() + interval '1 hour')`,
		id, h.org, h.program, kind)
	return id, h.signer.Mint(auth.PurposeReport, id, time.Now().Add(time.Hour))
}

func (h *harness) post(t *testing.T, token string, report any) *http.Response {
	t.Helper()

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/reports", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func report(name string, completed bool) ingest.Report {
	return ingest.Report{
		SchemaVersion: "1.0",
		Run:           ingest.RunInfo{Input: "domain", Scope: "full", Completed: completed, Version: "1.2.3"},
		Hosts: []ingest.Host{{
			Host: name, Status: ingest.StatusLive,
			Addresses: []string{"93.184.216.34"},
			Ports: []ingest.Port{{Port: 443, Protocol: "tcp", State: "open",
				HTTP: &ingest.HTTP{URL: "https://" + name, Scheme: "https", StatusCode: 200}}},
		}},
	}
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAReportIsIngestedAndClosesItsRun(t *testing.T) {
	h := newHarness(t)
	runID, token := h.run(t, "discovery")

	resp := h.post(t, token, report("api.target.test", true))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if n := h.count(t, `SELECT count(*) FROM asset WHERE org_id = $1`, h.org); n == 0 {
		t.Error("the report wrote no asset")
	}

	var state string
	var started, finished *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, started_at, finished_at FROM run WHERE id = $1`, runID).
		Scan(&state, &started, &finished); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if state != "completed" {
		t.Errorf("run state = %s, want completed", state)
	}
	// started_at is the only thing separating a run something opened from one
	// whose provisioning failed, and those call for opposite actions.
	if started == nil {
		t.Error("started_at was not written by the first report")
	}
}

// A truncated run is not a failed one, and it is not a completed one either.
func TestATruncatedRunSaysSoRatherThanFailing(t *testing.T) {
	h := newHarness(t)
	runID, token := h.run(t, "discovery")

	if resp := h.post(t, token, report("api.target.test", false)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// What it wrote is kept either way: running out of time is data, not an
	// error, and the assets it did reach are real.
	if n := h.count(t, `SELECT count(*) FROM asset`); n == 0 {
		t.Error("a truncated report wrote nothing")
	}

	// It delivered, so it is not failed: a scheduler reading "failed" would
	// re-run work whose results it already holds. What it said about its own
	// completeness is in the summary, where it stays legible.
	var state string
	var summary map[string]any
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, summary FROM run WHERE id = $1`, runID).Scan(&state, &summary); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if state == "failed" {
		t.Error("a run that delivered a truncated report was closed as failed")
	}
	if summary["completed"] != false {
		t.Errorf("summary completed = %v, want false: a run that delivered nine hundred "+
			"hosts before its deadline has to stay distinguishable from one that crashed "+
			"on the first", summary["completed"])
	}
}

// A major version this does not know has to be refused. The report type is
// transcribed rather than shared so the scanner can evolve, and that only holds
// if a document reusing field names under new meanings is a 400 rather than
// wrong inventory written in silence.
func TestAnUnknownReportSchemaIsRefused(t *testing.T) {
	h := newHarness(t)
	_, token := h.run(t, "discovery")

	future := report("api.target.test", true)
	future.SchemaVersion = "2.0"

	if resp := h.post(t, token, future); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if n := h.count(t, `SELECT count(*) FROM asset`); n != 0 {
		t.Errorf("%d assets were written under the wrong schema", n)
	}

	// A minor bump adds fields, which the unknown-field counter handles.
	minor := report("api.target.test", true)
	minor.SchemaVersion = "1.7"
	if resp := h.post(t, token, minor); resp.StatusCode != http.StatusOK {
		t.Errorf("a minor bump: status %d, want 200", resp.StatusCode)
	}
}

// The data is still valid: the run may have been re-dispatched, and
// deduplication merges the two. Refusing it would throw away work that was
// actually done.
func TestALateReportIsAcceptedAndMarked(t *testing.T) {
	h := newHarness(t)
	runID, token := h.run(t, "discovery")
	h.exec(t, `UPDATE run SET deadline = now() - interval '1 hour' WHERE id = $1`, runID)

	if resp := h.post(t, token, report("api.target.test", true)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if n := h.count(t, `SELECT count(*) FROM asset`); n == 0 {
		t.Error("a late report wrote nothing, and the work it describes was really done")
	}

	var summary map[string]any
	if err := h.pool.QueryRow(context.Background(),
		`SELECT summary FROM run WHERE id = $1`, runID).Scan(&summary); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if summary["Late"] != true {
		t.Errorf("the report was not marked late: %v", summary)
	}
}

// A run that started before an expiry must not write after it.
func TestAReportOnAnExpiredProgramIsRejected(t *testing.T) {
	h := newHarness(t)
	_, token := h.run(t, "discovery")

	h.exec(t, `UPDATE program SET authorized_to = now() - interval '1 minute' WHERE id = $1`, h.program)

	resp := h.post(t, token, report("api.target.test", true))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
	if n := h.count(t, `SELECT count(*) FROM asset`); n != 0 {
		t.Errorf("%d assets were written for an expired programme", n)
	}
}

func TestAReportOnASuspendedProgramIsRejected(t *testing.T) {
	h := newHarness(t)
	_, token := h.run(t, "discovery")

	h.exec(t, `UPDATE program SET state = 'suspended' WHERE id = $1`, h.program)

	if resp := h.post(t, token, report("api.target.test", true)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

// The effective revocation. A signed token cannot be recalled, so it stays
// valid and stops being useful.
func TestATerminalRunAcceptsNothingFurther(t *testing.T) {
	h := newHarness(t)
	runID, token := h.run(t, "discovery")

	if resp := h.post(t, token, report("api.target.test", true)); resp.StatusCode != http.StatusOK {
		t.Fatalf("first post: status %d", resp.StatusCode)
	}
	resp := h.post(t, token, report("other.target.test", true))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status %d, want 409 on a closed run", resp.StatusCode)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE key = 'other.target.test'`); n != 0 {
		t.Error("a closed run wrote again")
	}
	_ = runID
}

func TestTheCredentialIsRequiredAndBound(t *testing.T) {
	h := newHarness(t)
	runID, _ := h.run(t, "discovery")

	if resp := h.post(t, "", report("api.target.test", true)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: status %d, want 401", resp.StatusCode)
	}
	if resp := h.post(t, "not-a-token", report("api.target.test", true)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forged credential: status %d, want 401", resp.StatusCode)
	}

	// Minted for the other purpose. Replaying a target-list credential to post
	// a report has to fail.
	targets := h.signer.Mint(auth.PurposeTargets, runID, time.Now().Add(time.Hour))
	if resp := h.post(t, targets, report("api.target.test", true)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a targets credential: status %d, want 401", resp.StatusCode)
	}

	// And one naming a run that does not exist answers the same way, so the
	// endpoint is not an oracle for which runs an organization has.
	unknown := h.signer.Mint(auth.PurposeReport, uuid.New(), time.Now().Add(time.Hour))
	if resp := h.post(t, unknown, report("api.target.test", true)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unknown run: status %d, want 401", resp.StatusCode)
	}
}
