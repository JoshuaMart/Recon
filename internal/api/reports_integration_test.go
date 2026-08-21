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
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/store"
)

const signingKey = "a-signing-key-long-enough-to-be-one"

type harness struct {
	pool    *pgxpool.Pool
	server  *httptest.Server
	signer  *auth.Signer
	org     uuid.UUID
	program uuid.UUID
}

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

	h := &harness{pool: pool, signer: signer, org: uuid.New(), program: uuid.New()}
	h.server = httptest.NewServer(api.NewReports(pool, signer, ingest.New(nil, quiet), quiet))
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

	var state string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state FROM run WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if state == "completed" {
		t.Error("a run that reported itself incomplete was closed as completed")
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
