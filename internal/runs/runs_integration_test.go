//go:build integration

// Milestone 2, the half a scheduler answers: the lease, its expiry, and what a
// refusal has to say.
package runs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

func pgStamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

type harness struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
	org     uuid.UUID
	program uuid.UUID
	clock   *clock
	sched   *runs.Scheduler
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

	signer, err := auth.NewSigner("a-signing-key-long-enough-to-be-one")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	cfg := config.Defaults().Verification
	cfg.PublicURL = "https://recon.example"
	cfg.BatchSize = 10

	h := &harness{
		pool:    pool,
		queries: sqlcgen.New(pool),
		org:     uuid.New(),
		program: uuid.New(),
		clock:   c,
		sched:   runs.New(signer, cfg, quiet, runs.WithClock(c.Now)),
	}

	exec(t, pool, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, h.org)
	exec(t, pool, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', $3)`,
		h.program, h.org, c.now.Add(-time.Hour))
	return h
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// due writes hosts that are already past their resolve date.
func (h *harness) due(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		id := uuid.New()
		exec(t, h.pool, `INSERT INTO asset
			(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,$4,'manual','in_scope',$5,$5)`,
			id, h.org, h.program, name, h.clock.now.Add(-time.Hour))
		exec(t, h.pool, `INSERT INTO asset_current
			(asset_id, org_id, program_id, kind, key, scope_status, host,
			 next_resolve_at, next_full_at, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,'in_scope',$4,$5,$5,$5,$5)`,
			id, h.org, h.program, name, h.clock.now.Add(-time.Hour))
	}
}

// The lease, and the whole of it. Selection skips what a live run already
// holds, so two runs never scan the same host at the same time.
func TestTwoConcurrentRunsNeverHoldTheSameHost(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "one.acme.test", "two.acme.test")

	first, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.TargetCount != 2 {
		t.Fatalf("the first run froze %d targets", first.TargetCount)
	}

	// A second run of the same kind is refused outright, and the message has
	// to name the run somebody has to decide about.
	_, err = h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err == nil {
		t.Fatal("a second run was provisioned while one was in flight")
	}
	if !strings.Contains(err.Error(), first.RunID.String()) {
		t.Fatalf("the refusal does not name the run: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing has opened it") {
		t.Fatalf("the refusal does not say whether anything claimed it: %v", err)
	}

	// And the hosts it froze are invisible to a selection, which is what the
	// exclusion in the statement is for rather than the refusal above.
	exec(t, h.pool, `UPDATE run SET kind = 'discovery' WHERE id = $1`, first.RunID)
	second, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err != runs.ErrNothingDue {
		t.Fatalf("a second run selected %v, want nothing due", second)
	}
}

// A run that dies takes nothing with it. Due dates move only when a report is
// ingested, so an abandoned run leaves the inventory exactly as it found it and
// the next tick selects the same assets again.
func TestARunKilledMidFlightBlocksNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "one.acme.test", "two.acme.test")

	first, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Nothing ever reports. The deadline passes.
	h.clock.now = h.clock.now.Add(h.deadlineOf(t, first.RunID).Sub(h.clock.now) + time.Minute)
	expired, err := h.sched.Sweep(ctx, h.queries)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if expired != 1 {
		t.Fatalf("%d runs expired", expired)
	}

	// The failure is visible rather than repaired, and the targets are free.
	var state, reason string
	if err := h.pool.QueryRow(ctx, `SELECT state, error FROM run WHERE id = $1`, first.RunID).
		Scan(&state, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if state != "expired" || reason == "" {
		t.Fatalf("the run is %q with error %q", state, reason)
	}

	second, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.TargetCount != 2 {
		t.Fatalf("the next tick selected %d of the 2 hosts the dead run held", second.TargetCount)
	}
}

func (h *harness) deadlineOf(t *testing.T, run uuid.UUID) time.Time {
	t.Helper()

	var deadline time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT deadline FROM run WHERE id = $1`, run).Scan(&deadline); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	return deadline
}

// An asset due for full does not need a resolve run, and the reverse is not
// true. The rung a selection asks for is the rung it reads.
func TestSelectionReadsTheRungItWasAskedFor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "both.acme.test")
	exec(t, h.pool, `UPDATE asset_current SET next_full_at = $1 WHERE key = 'both.acme.test'`,
		h.clock.now.Add(48*time.Hour))

	full, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "full")
	if err != runs.ErrNothingDue {
		t.Fatalf("a host due only for resolve was selected for a full run: %v %v", full, err)
	}

	resolve, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve")
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	if resolve.Env["FASTRECON_STAGES"] != "resolve" {
		t.Fatalf("the stages are %q", resolve.Env["FASTRECON_STAGES"])
	}
}

// Nothing a run holds opens the inventory. The definition carries two
// signatures over the run, its purpose and an expiry, and no database
// credential of any kind.
func TestARunDefinitionCarriesNoInventoryCredential(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "one.acme.test")

	def, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "full")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.HasPrefix(def.Env["FASTRECON_TARGETS_URL"], "https://recon.example/runs/"+def.RunID.String()) {
		t.Fatalf("the target list is at %q", def.Env["FASTRECON_TARGETS_URL"])
	}
	if !strings.Contains(def.Env["FASTRECON_WEBHOOK_HEADER"], "Bearer ") {
		t.Fatalf("the report credential reads %q", def.Env["FASTRECON_WEBHOOK_HEADER"])
	}
	if def.Env["FASTRECON_PORTS"] == "" {
		t.Fatal("the port list is data and it travels in the definition, so discovery and verification scan the same ports")
	}
	for key, value := range def.Env {
		for _, forbidden := range []string{"postgres://", "postgresql://", "password"} {
			if strings.Contains(strings.ToLower(value), forbidden) {
				t.Fatalf("%s carries %q", key, forbidden)
			}
		}
	}
	// The two credentials are for different things, and one must not open the
	// other.
	targets := def.Env["FASTRECON_TARGETS_URL"]
	token := targets[strings.Index(targets, "token=")+len("token="):]
	signer, _ := auth.NewSigner("a-signing-key-long-enough-to-be-one")
	if _, err := signer.Verify(auth.PurposeReport, token, h.clock.now); err == nil {
		t.Fatal("a token minted to fetch a target list was accepted to post a report")
	}
}

// The authorization window is checked when a run is provisioned rather than
// only when its report arrives. Without it an expired programme would be
// provisioned on every tick and refused when the run opens: an execution billed
// to do nothing.
func TestAnExpiredProgrammeIsNeverProvisioned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "one.acme.test")
	exec(t, h.pool, `UPDATE program SET authorized_to = $1 WHERE id = $2`,
		h.clock.now.Add(-time.Minute), h.program)

	if _, err := h.sched.Verification(ctx, h.queries, h.org, h.program, "resolve"); err == nil {
		t.Fatal("a run was provisioned over an expired authorization")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("the refusal reads %v", err)
	}
}

// last_discovery_at is written at creation rather than at completion. A run
// that dies on the way must not be restarted by the cadence: the sweeper
// already handles it, and confusing the two would start two.
func TestDiscoveryIsRecordedWhenItIsProvisioned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.sched.Discovery(ctx, h.queries, h.org, h.program); err != runs.ErrNoPerimeter {
		t.Fatalf("a programme with no apex was provisioned: %v", err)
	}

	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'include','apex','acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))

	def, err := h.sched.Discovery(ctx, h.queries, h.org, h.program)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if def.Env["FASTRECON_DOMAIN"] != "acme.test" {
		t.Fatalf("the perimeter is %q", def.Env["FASTRECON_DOMAIN"])
	}
	// A discovery run gets no targets URL and a domain instead. That is the
	// whole difference between the two mandates.
	if _, present := def.Env["FASTRECON_TARGETS_URL"]; present {
		t.Fatal("a discovery run was handed a target list, which is the one thing that makes an absence mean something")
	}

	var last time.Time
	if err := h.pool.QueryRow(ctx, `SELECT last_discovery_at FROM program WHERE id = $1`, h.program).
		Scan(&last); err != nil {
		t.Fatalf("read last_discovery_at: %v", err)
	}
	if !last.Equal(h.clock.now) {
		t.Fatalf("last_discovery_at is %s, want %s", last, h.clock.now)
	}

	// And the pass that provisions on a cadence sees the run in flight.
	programs, err := h.queries.ProgramsDueForDiscovery(ctx, sqlcgen.ProgramsDueForDiscoveryParams{
		At: pgStamp(h.clock.now.Add(30 * 24 * time.Hour)),
	})
	if err != nil {
		t.Fatalf("read due programmes: %v", err)
	}
	if len(programs) != 0 {
		t.Fatalf("%d programmes were due while a discovery run was in flight", len(programs))
	}
}

// The in-flight check is a read followed by a write, and a transaction cannot
// see another's uncommitted rows. Two of them overlapping both pass the check
// and both freeze the same hosts, which is double scan traffic against
// somebody's perimeter.
//
// The overlap is forced rather than raced: the second transaction takes its
// snapshot before the first commits and holds it, which is exactly the window
// the check has and a sleep would only sometimes reproduce. Discovery has
// carried a partial unique index since the first migration; verification did
// not, and this is the assertion that says so.
func TestTwoOverlappingRunsCannotBothBeCreated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "one.acme.test", "two.acme.test")

	first, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = first.Rollback(ctx) }()

	second, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = second.Rollback(ctx) }()

	if _, err := h.sched.Verification(ctx, sqlcgen.New(first), h.org, h.program, "resolve"); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	// Taken while the first is still uncommitted, and held.
	if _, err := second.Exec(ctx, `SELECT count(*) FROM run`); err != nil {
		t.Fatalf("fix the snapshot: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first: %v", err)
	}

	// The second still sees a programme with nothing in flight and hosts
	// nothing holds. Only the write can stop it.
	_, err = h.sched.Verification(ctx, sqlcgen.New(second), h.org, h.program, "resolve")
	if err == nil {
		t.Fatal("two runs were created over the same hosts, and each of them will scan those hosts")
	}
	if !errors.Is(err, runs.ErrRunInFlight) {
		t.Fatalf("the loser reports %v, and a caller has to be able to tell this from a failure", err)
	}

	if n := h.count(t, `SELECT count(*) FROM run WHERE program_id = $1 AND state IN ('pending','running')`,
		h.program); n != 1 {
		t.Fatalf("%d live runs", n)
	}
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}
