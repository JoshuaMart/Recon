//go:build integration

// Milestone 1, in the assertions a database can answer.
package ingest_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

type harness struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
	counter *store.QueryCounter
	org     uuid.UUID
	program uuid.UUID
	ing     *ingest.Ingestor
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

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	counter := &store.QueryCounter{}
	cfg.ConnConfig.Tracer = counter

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	h := &harness{
		pool:    pool,
		queries: sqlcgen.New(pool),
		counter: counter,
		org:     uuid.New(),
		program: uuid.New(),
		ing:     ingest.New(nil, quiet),
	}

	exec(t, pool, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, h.org)
	exec(t, pool, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', now())`,
		h.program, h.org)
	return h
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (h *harness) run() ingest.Run {
	due := time.Now()
	return ingest.Run{
		ID: uuid.New(), OrgID: h.org, ProgramID: h.program, Kind: "discovery",
		Due: ingest.Schedule{Resolve: &due},
	}
}

func (h *harness) scope(t *testing.T, rules ...scope.Rule) *scope.Set {
	t.Helper()

	set, err := scope.Compile(rules)
	if err != nil {
		t.Fatalf("compile scope: %v", err)
	}
	return set
}

func include(pattern string) scope.Rule {
	return scope.Rule{ID: "include:" + pattern, Kind: scope.Include, Matcher: scope.MatchApex, Pattern: pattern}
}

func exclude(matcher, pattern string) scope.Rule {
	return scope.Rule{ID: "exclude:" + pattern, Kind: scope.Exclude, Matcher: matcher, Pattern: pattern}
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// oneHost is the smallest report that exercises every layer.
func oneHost(name string) ingest.Report {
	return ingest.Report{
		SchemaVersion: "1.0",
		Run:           ingest.RunInfo{Input: "domain", Scope: "full", Completed: true, Version: "1.2.3"},
		Hosts: []ingest.Host{{
			Host: name, Status: ingest.StatusLive,
			Addresses: []string{"93.184.216.34"},
			Sources:   []string{"crt"},
			Ports: []ingest.Port{{
				Port: 443, Protocol: "tcp", State: "open",
				Addresses: []string{"93.184.216.34"},
				HTTP: &ingest.HTTP{
					URL: "https://" + name, Scheme: "https", StatusCode: 200,
					Title: "Home", Tech: []string{"nginx"},
					// Both differ on every pass, and both are dropped.
					ResponseTimeMS: 251, ContentLength: 1533,
				},
			}},
		}},
	}
}

func TestPostingTheSameReportTwiceCreatesOneSeriesOfObservations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))
	report := oneHost("api.target.test")

	first, err := h.ing.Report(ctx, h.queries, h.run(), set, report)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	second, err := h.ing.Report(ctx, h.queries, h.run(), set, report)
	if err != nil {
		t.Fatalf("second report: %v", err)
	}

	if second.Observations != first.Observations {
		t.Fatalf("the two reports wrote different numbers of observations: %d then %d",
			first.Observations, second.Observations)
	}
	if second.Deduplicated != second.Observations {
		t.Errorf("%d of %d observations deduplicated on the replay, want all of them",
			second.Deduplicated, second.Observations)
	}

	rows := h.count(t, `SELECT count(*) FROM observation`)
	if rows != first.Observations {
		t.Errorf("%d rows for %d observations submitted twice, want %d",
			rows, first.Observations*2, first.Observations)
	}
}

func TestAThousandIdenticalObservationsCreateOneRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))
	report := oneHost("api.target.test")

	for range 1000 {
		if _, err := h.ing.Report(ctx, h.queries, h.run(), set, report); err != nil {
			t.Fatalf("report: %v", err)
		}
	}

	var rows int
	var observed, confirmed time.Time
	err := h.pool.QueryRow(ctx, `
		SELECT count(*) OVER (), min(observed_at) OVER (), max(last_confirmed_at) OVER ()
		  FROM observation WHERE layer = 'dns' LIMIT 1`).Scan(&rows, &observed, &confirmed)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if rows != 1 {
		t.Errorf("%d rows on the dns layer, want 1", rows)
	}
	// Each row means "this state held from observed_at to last_confirmed_at",
	// so the window has to have widened rather than the row been rewritten.
	if !confirmed.After(observed) {
		t.Errorf("the confirmation window did not widen: %s to %s", observed, confirmed)
	}
}

func TestTheDeduplicationRateClearsItsThreshold(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))

	var reports []ingest.Report
	for i := range 50 {
		reports = append(reports, oneHost(fmt.Sprintf("host%02d.target.test", i)))
	}

	var submitted, deduplicated int
	// Twenty passes rather than ten: the first can structurally deduplicate
	// nothing, so at N passes the arithmetic ceiling is (N-1)/N, and ten would
	// put that ceiling exactly on the threshold.
	for range 20 {
		for _, report := range reports {
			summary, err := h.ing.Report(ctx, h.queries, h.run(), set, report)
			if err != nil {
				t.Fatalf("report: %v", err)
			}
			submitted += summary.Observations
			deduplicated += summary.Deduplicated
		}
	}

	rate := float64(deduplicated) / float64(submitted)
	if rate < 0.90 {
		t.Errorf("deduplication rate %.3f, want above 0.90: a drop means a regressed "+
			"normalization or a producer emitting an unfiltered volatile field", rate)
	}
	t.Logf("deduplication rate %.3f over %d submitted observations", rate, submitted)
}

func TestAnOutOfScopeAssetIsStoredAndMarked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"), exclude(scope.MatchFQDN, "admin.target.test"))

	for _, name := range []string{"api.target.test", "admin.target.test", "cdn.thirdparty.test"} {
		if _, err := h.ing.Report(ctx, h.queries, h.run(), set, oneHost(name)); err != nil {
			t.Fatalf("report %s: %v", name, err)
		}
	}

	// Kept rather than filtered at the source, which is what most tools lose
	// for good.
	for name, want := range map[string]string{
		"api.target.test":     "in_scope",
		"admin.target.test":   "out_of_scope",
		"cdn.thirdparty.test": "unknown",
	} {
		var got string
		if err := h.pool.QueryRow(ctx,
			`SELECT scope_status FROM asset WHERE key = $1`, name).Scan(&got); err != nil {
			t.Fatalf("%s was not stored at all: %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}

	// And only the in-scope one is scheduled.
	scheduled := h.count(t, `
		SELECT count(*) FROM asset_current WHERE next_resolve_at IS NOT NULL AND scope_status <> 'in_scope'`)
	if scheduled != 0 {
		t.Errorf("%d assets outside the perimeter carry a due date, and they would go on being scanned", scheduled)
	}
}

// The assertion the host-based classification exists for: a rule names a host,
// and matching it against a key would leave every service on that host out.
func TestARuleNamingAHostMovesItsServicesWithIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	before := h.scope(t, include("target.test"))
	if _, err := h.ing.Report(ctx, h.queries, h.run(), before, oneHost("api.target.test")); err != nil {
		t.Fatalf("report: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE kind = 'service'`); n != 1 {
		t.Fatalf("%d services derived, want 1: an open port has to become an asset", n)
	}

	// The rule changes, and nothing is rescanned.
	after := h.scope(t, include("target.test"), exclude(scope.MatchFQDN, "api.target.test"))
	due := time.Now()
	result, err := h.ing.Reclassify(ctx, h.queries, h.program, after, ingest.Schedule{Resolve: &due})
	if err != nil {
		t.Fatalf("reclassify: %v", err)
	}
	if result.Moved < 2 {
		t.Errorf("%d assets moved, want the host and its service: a service left behind "+
			"keeps its due dates and goes on being scanned", result.Moved)
	}

	out := h.count(t, `SELECT count(*) FROM asset WHERE scope_status = 'out_of_scope'`)
	if out != 2 {
		t.Errorf("%d assets out of scope, want the host and its service", out)
	}
	stillScheduled := h.count(t, `
		SELECT count(*) FROM asset_current WHERE scope_status = 'out_of_scope' AND next_resolve_at IS NOT NULL`)
	if stillScheduled != 0 {
		t.Errorf("%d excluded assets kept their due dates", stillScheduled)
	}
}

func TestAURLPrefixExclusionLeavesItsServiceInScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	set := h.scope(t, include("target.test"),
		exclude(scope.MatchURLPrefix, "https://app.target.test/internal/"))
	if _, err := h.ing.Report(ctx, h.queries, h.run(), set, oneHost("app.target.test")); err != nil {
		t.Fatalf("report: %v", err)
	}

	var status string
	if err := h.pool.QueryRow(ctx,
		`SELECT scope_status FROM asset WHERE kind = 'service'`).Scan(&status); err != nil {
		t.Fatalf("read the service: %v", err)
	}
	if status != "in_scope" {
		t.Errorf("the service carrying an excluded path = %s, want in_scope: a url_prefix "+
			"rule is more specific than a host, so a child may be stricter than its parent", status)
	}
}

// Two organizations tracking the same public target hold two independent
// inventories: the authorization is granted per organization, and a shared one
// would be an information leak.
func TestTwoOrganizationsHoldTheSameHostIndependently(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))

	other := uuid.New()
	otherProgram := uuid.New()
	exec(t, h.pool, `INSERT INTO org (id, name) VALUES ($1, 'other')`, other)
	exec(t, h.pool, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', now())`,
		otherProgram, other)

	if _, err := h.ing.Report(ctx, h.queries, h.run(), set, oneHost("api.target.test")); err != nil {
		t.Fatalf("first tenant: %v", err)
	}
	second := h.run()
	second.OrgID, second.ProgramID = other, otherProgram
	if _, err := h.ing.Report(ctx, h.queries, second, set, oneHost("api.target.test")); err != nil {
		t.Fatalf("second tenant: %v", err)
	}

	if n := h.count(t, `SELECT count(*) FROM asset WHERE key = 'api.target.test'`); n != 2 {
		t.Errorf("%d rows for one name across two organizations, want 2 independent ones", n)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset WHERE org_id = $1 AND key = 'api.target.test'`, h.org); n != 1 {
		t.Errorf("a query carrying one org_id returned %d rows, want its own", n)
	}
}

// It is what stops a scanner choosing its own perimeter, and it is a rejection
// rather than a silent skip.
func TestAHostOutsideTheFrozenListIsRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))

	run := h.run()
	run.Kind = "verification"
	run.Targets = map[string]struct{}{"api.target.test": {}}

	report := oneHost("api.target.test")
	report.Hosts = append(report.Hosts, ingest.Host{Host: "elsewhere.target.test", Status: ingest.StatusLive})

	summary, err := h.ing.Report(ctx, h.queries, run, set, report)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if summary.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", summary.Rejected)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE key = 'elsewhere.target.test'`); n != 0 {
		t.Errorf("the out-of-list host was written anyway (%d rows)", n)
	}
}

// The number is what keeps a first asset reaching the database quickly: at a
// millisecond of latency, two statements per observation against seven is two
// hundred milliseconds of waiting against seven hundred.
func TestTheIngestionCostStaysUnderItsBudget(t *testing.T) {
	const budget = 3.0

	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))

	// Warm the connection so the pool's own setup is not counted as ingestion.
	if _, err := h.ing.Report(ctx, h.queries, h.run(), set, oneHost("warmup.target.test")); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	var reports []ingest.Report
	for i := range 100 {
		reports = append(reports, oneHost(fmt.Sprintf("host%03d.target.test", i)))
	}

	h.counter.Reset()
	var observations int
	for _, report := range reports {
		summary, err := h.ing.Report(ctx, h.queries, h.run(), set, report)
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		observations += summary.Observations
	}

	perObservation := float64(h.counter.Count()) / float64(observations)
	t.Logf("%d round trips for %d observations: %.2f each",
		h.counter.Count(), observations, perObservation)
	if perObservation > budget {
		t.Errorf("%.2f round trips per observation, budget %.1f: somebody added a query "+
			"to the write path", perObservation, budget)
	}
}

var _ = pgtype.Timestamptz{}

// The projection carries the operator, and it travels with the upsert rather
// than in a statement of its own: the enrichment is in hand at the moment the
// row is written.
func TestTheOperatorReachesTheProjection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := h.scope(t, include("target.test"))

	h.ing = ingest.New(fixedEnricher{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := h.ing.Report(ctx, h.queries, h.run(), set, oneHost("api.target.test")); err != nil {
		t.Fatalf("report: %v", err)
	}

	var asn *int32
	var org, country *string
	err := h.pool.QueryRow(ctx,
		`SELECT asn, asn_org, country FROM asset_current WHERE key = 'api.target.test'`).
		Scan(&asn, &org, &country)
	if err != nil {
		t.Fatalf("read the projection: %v", err)
	}
	if asn == nil || *asn != 15133 {
		t.Errorf("asn = %v, want the operator the enricher reported", asn)
	}
	if org == nil || *org != "Example Networks" {
		t.Errorf("asn_org = %v", org)
	}
	if country == nil || *country != "FR" {
		t.Errorf("country = %v", country)
	}

	// A service derived from that host carries it too: five ports of one
	// address have the same operator, and the row that describes a surface is
	// the one somebody filters on.
	var serviceASN *int32
	if err := h.pool.QueryRow(ctx,
		`SELECT asn FROM asset_current WHERE kind = 'service'`).Scan(&serviceASN); err != nil {
		t.Fatalf("read the service: %v", err)
	}
	if serviceASN == nil {
		t.Error("the derived service carries no operator, and it is the row a filter reads")
	}
}

// A stand-in rather than the real databases: this asserts the wiring, and the
// databases have their own test that skips when they are absent.
type fixedEnricher struct{}

func (fixedEnricher) Configured() bool { return true }
func (fixedEnricher) Close() error     { return nil }
func (fixedEnricher) Lookup(netip.Addr) enrich.Result {
	return enrich.Result{ASN: 15133, ASNOrg: "Example Networks", Country: "FR", City: "Paris"}
}
