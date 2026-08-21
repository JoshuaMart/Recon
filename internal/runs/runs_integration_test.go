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
	// The flag takes the scanner's own scope name, which is the same value the
	// run row is constrained to. Anything else is a run that fails on its
	// configuration a second after it starts, which no test here could catch
	// because both sides of the invention live in this repository.
	if arg(resolve.Args, "--stages") != "resolve" {
		t.Fatalf("the stages are %q", arg(resolve.Args, "--stages"))
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

	list := arg(def.Args, "--targets-url")
	if list != "https://recon.example/runs/"+def.RunID.String()+"/targets" {
		t.Fatalf("the target list is at %q", list)
	}
	// And it carries no credential. A token in a query string is a token in
	// every access log, proxy log and error message that ever prints that URL,
	// and those outlive the run by a long way.
	if strings.Contains(list, "token") || strings.Contains(list, "?") {
		t.Fatalf("the target list URL carries its credential: %q", list)
	}
	if !strings.HasPrefix(arg(def.Args, "--targets-header"), "Authorization: Bearer ") {
		t.Fatalf("the list credential reads %q", arg(def.Args, "--targets-header"))
	}
	if !strings.HasPrefix(arg(def.Args, "--webhook-header"), "Authorization: Bearer ") {
		t.Fatalf("the report credential reads %q", arg(def.Args, "--webhook-header"))
	}
	if arg(def.Args, "--ports") == "" {
		t.Fatal("the port list is data and it travels in the definition, so discovery and verification scan the same ports")
	}
	for _, value := range def.Args {
		for _, forbidden := range []string{"postgres://", "postgresql://", "password"} {
			if strings.Contains(strings.ToLower(value), forbidden) {
				t.Fatalf("an argument carries %q", forbidden)
			}
		}
	}
	// The two credentials are for different things, and one must not open the
	// other.
	token := strings.TrimPrefix(arg(def.Args, "--targets-header"), "Authorization: Bearer ")
	signer, _ := auth.NewSigner("a-signing-key-long-enough-to-be-one")
	if _, err := signer.Verify(auth.PurposeReport, token, h.clock.now); err == nil {
		t.Fatal("a token minted to fetch a target list was accepted to post a report")
	}
}

// arg reads a flag's value out of an invocation.
func arg(args []string, flag string) string {
	for i, value := range args {
		if value == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
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
	if arg(def.Args, "--stages") != runs.ScopeFull {
		t.Fatalf("a discovery run walks %q of the ladder", arg(def.Args, "--stages"))
	}
	if arg(def.Args, "-d") != "acme.test" {
		t.Fatalf("the perimeter is %q", arg(def.Args, "-d"))
	}
	// A discovery run gets no targets URL and a domain instead. That is the
	// whole difference between the two mandates.
	if arg(def.Args, "--targets-url") != "" {
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

// A discovery run that never completes is expired and a replacement is
// provisioned.
//
// last_discovery_at is written at creation so the cadence cannot start a second
// run while the first is in flight. Left there after a run died, it also means
// the programme waits a whole discovery interval for a replacement: a run that
// failed in thirty seconds would cost a week of coverage.
func TestADeadDiscoveryRunIsReplacedRatherThanWaitedOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'include','apex','acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))

	// Through the cadence, because that is where the interval is read.
	// Calling Discovery directly would answer a different question: it checks
	// what is in flight and never what the cadence allows.
	platform := &recorder{id: "run-01"}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(platform))
	cadence := runs.NewCadence(h.pool, scheduler, time.Minute, quiet())

	if started, err := cadence.Once(ctx); err != nil || started != 1 {
		t.Fatalf("the first run: %d started, %v", started, err)
	}

	var deadline time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT deadline FROM run WHERE program_id = $1`, h.program).Scan(&deadline); err != nil {
		t.Fatalf("read the deadline: %v", err)
	}

	// Nothing ever reports. The deadline passes.
	h.clock.now = deadline.Add(time.Minute)
	if _, err := h.sched.Sweep(ctx, h.queries); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// A whole discovery interval has not passed, and the replacement still
	// goes out: a run that failed in thirty seconds must not cost a week.
	started, err := cadence.Once(ctx)
	if err != nil {
		t.Fatalf("the replacement: %v", err)
	}
	if started != 1 {
		t.Fatalf("%d replacements went out, and the interval is seven days away", started)
	}

	// And a run in flight still bounds the cadence to one at a time, which is
	// the property clearing the slot must not undo.
	if again, err := cadence.Once(ctx); err != nil || again != 0 {
		t.Fatalf("a third tick started %d runs while one was in flight, %v", again, err)
	}
}

// The cadence provisions and starts, and it is the platform that says what the
// execution was called.
func TestTheCadenceStartsWhatItProvisions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'include','apex','acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))
	// An exclusion travels with the perimeter, as the second safety net in
	// front of the network.
	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'exclude','fqdn','vpn.acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))

	platform := &recorder{id: "run-01"}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(platform))

	started, err := runs.NewCadence(h.pool, scheduler, time.Minute, quiet()).Once(ctx)
	if err != nil {
		t.Fatalf("cadence: %v", err)
	}
	if started != 1 {
		t.Fatalf("%d runs went out", started)
	}
	if platform.calls != 1 {
		t.Fatalf("the platform was called %d times", platform.calls)
	}
	if arg(platform.args, "--exclude") != "vpn.acme.test" {
		t.Fatalf("the exclusions did not travel: %v", platform.args)
	}

	var external string
	if err := h.pool.QueryRow(ctx,
		`SELECT external_id FROM run WHERE program_id = $1 AND kind = 'discovery'`,
		h.program).Scan(&external); err != nil {
		t.Fatalf("read the external id: %v", err)
	}
	if external != "run-01" {
		t.Fatalf("the platform's name for the run is %q, and without it its logs are unfindable", external)
	}

	// A second tick starts nothing: the run is in flight.
	again, err := runs.NewCadence(h.pool, scheduler, time.Minute, quiet()).Once(ctx)
	if err != nil {
		t.Fatalf("cadence: %v", err)
	}
	if again != 0 || platform.calls != 1 {
		t.Fatalf("a second tick started %d runs, %d platform calls", again, platform.calls)
	}
}

// A platform that refuses leaves a row the deadline sweeper owns, not nothing.
func TestAPlatformRefusalLeavesTheSweeperSomethingToOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'include','apex','acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))

	platform := &recorder{err: errors.New("quota exceeded")}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(platform))

	started, err := runs.NewCadence(h.pool, scheduler, time.Minute, quiet()).Once(ctx)
	if err != nil {
		t.Fatalf("cadence: %v", err)
	}
	if started != 0 {
		t.Fatalf("%d runs were reported started against a platform that refused", started)
	}

	var state string
	if err := h.pool.QueryRow(ctx,
		`SELECT state FROM run WHERE program_id = $1`, h.program).Scan(&state); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if state != "pending" {
		t.Fatalf("the run is %q, and a refusal has to leave the sweeper something to expire", state)
	}
}

// recorder stands in for a platform.
type recorder struct {
	id    string
	err   error
	calls int
	args  []string
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Start(_ context.Context, def *runs.Definition) (string, error) {
	r.calls++
	r.args = def.Args
	return r.id, r.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
