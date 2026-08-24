//go:build integration

package ct_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/ct"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store"
)

var at = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

type harness struct {
	pool    *pgxpool.Pool
	org     uuid.UUID
	program uuid.UUID
	now     time.Time
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

	migrator, err := store.NewMigrator(url, quiet())
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

	h := &harness{pool: pool, org: uuid.New(), program: uuid.New(), now: at}
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, h.org)
	h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', $3)`,
		h.program, h.org, at.Add(-time.Hour))
	return h
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (h *harness) rule(t *testing.T, kind, matcher, pattern string) {
	t.Helper()

	h.exec(t, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
	           VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), h.org, h.program, kind, matcher, pattern, at.Add(-time.Hour))
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// matcher builds one whose clock the test owns.
func (h *harness) matcher(opts ct.Options) *ct.Matcher {
	ingestor := ingest.New(nil, lifecycle.DefaultCadence(), quiet(),
		ingest.WithClock(func() time.Time { return h.now }))
	return ct.New(h.pool, store.NewScoped(h.pool), ingestor, opts, quiet()).
		WithClock(func() time.Time { return h.now })
}

func options() ct.Options {
	o := ct.DefaultOptions()
	o.Interval = time.Minute
	return o
}

// frame builds a certificate carrying these names, from the pinned stream so
// the shape is the real one rather than one this test invented.
func frame(t *testing.T, names ...string) *ct.Frame {
	t.Helper()

	file, err := os.Open("testdata/stream.jsonl")
	if err != nil {
		t.Fatalf("open the pinned stream: %v", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var f ct.Frame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			continue
		}
		if len(f.Data.LeafCert.AllDomains) == 0 || f.Data.Source.Name == "" {
			continue
		}
		f.Data.LeafCert.AllDomains = names
		return &f
	}
	t.Fatal("the pinned stream carries no usable frame")
	return nil
}

func TestTheSetHoldsApexIncludesOfAuthorizedProgrammesAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.rule(t, "include", "apex", "acme.test")
	// An include naming a host already declared it, so the asset exists and
	// carries a due date: this stream would find the same name and create
	// nothing.
	h.rule(t, "include", "fqdn", "www.other.test")
	// Naming something to take it out of a perimeter is not a reason to watch
	// the logs for it.
	h.rule(t, "exclude", "apex", "excluded.test")
	// A shape is not a thing, and the whole point is that there is no regex.
	h.rule(t, "include", "regex", `.*\.shape\.test`)

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Apexes(); got != 1 {
		t.Fatalf("the set holds %d apexes, and only the apex include belongs in it", got)
	}

	// An expired authorization drops out at the next reload. Left in, it keeps
	// creating assets with due dates on a perimeter nobody may scan, so the
	// first thing each one does is have its run refused.
	h.exec(t, `UPDATE program SET authorized_to = $1 WHERE id = $2`, at.Add(-time.Minute), h.program)
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Apexes(); got != 0 {
		t.Errorf("an expired programme still holds %d apexes in the set", got)
	}

	h.exec(t, `UPDATE program SET authorized_to = NULL, state = 'suspended' WHERE id = $1`, h.program)
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Apexes(); got != 0 {
		t.Errorf("a suspended programme still holds %d apexes in the set", got)
	}
}

func TestAMatchingSANBecomesACandidateAndAnUnmatchedOneNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m.Handle(ctx, frame(t, "staging.acme.test", "unrelated.example.org", "acme.test.evil.com"))

	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 1 {
		t.Fatalf("%d assets, and exactly one SAN was under the apex", n)
	}

	var key, source, life string
	var resolve, full *time.Time
	err := h.pool.QueryRow(ctx,
		`SELECT a.key, a.discovery_source, c.lifecycle, c.next_resolve_at, c.next_full_at
		   FROM asset a JOIN asset_current c ON c.asset_id = a.id
		  WHERE a.program_id = $1`, h.program).Scan(&key, &source, &life, &resolve, &full)
	if err != nil {
		t.Fatalf("read the candidate: %v", err)
	}
	if key != "staging.acme.test" {
		t.Errorf("the candidate is %q", key)
	}
	if source != ingest.SourceCertstream {
		t.Errorf("the discovery source is %q", source)
	}
	if life != "candidate" {
		t.Errorf("the lifecycle is %q", life)
	}
	if resolve == nil || !resolve.Equal(h.now) {
		t.Errorf("the resolve due date is %v, want %s", resolve, h.now)
	}
	if full != nil {
		t.Errorf("the candidate is scheduled for a full run at %s", full)
	}
}

// The milestone assertion, and the constraint is what answers it: the cache is
// a cost control and never the correctness one.
func TestASANSeenTenTimesInAMinuteCreatesOneAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	warm := h.matcher(options())
	if err := warm.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for range 10 {
		warm.Handle(ctx, frame(t, "repeated.acme.test"))
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 1 {
		t.Fatalf("%d assets with the cache warm", n)
	}

	// And with it cold every single time, which is what a restart between two
	// sightings looks like. If the cache were the guarantee, this is where it
	// would show.
	h.exec(t, `DELETE FROM asset_current`)
	h.exec(t, `DELETE FROM asset`)
	for range 10 {
		cold := h.matcher(options())
		if err := cold.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		cold.Handle(ctx, frame(t, "repeated.acme.test"))
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 1 {
		t.Fatalf("%d assets with the cache cold on every sighting", n)
	}
}

// Without a bound, a public feed decides the size of a customer's inventory,
// and that is one wrong apex rule away rather than hypothetical.
func TestPastItsCeilingAProgrammeCreatesNoFurtherCandidate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	opts := options()
	opts.Ceiling = 2
	m := h.matcher(opts)
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m.Handle(ctx, frame(t, "one.acme.test", "two.acme.test", "three.acme.test", "four.acme.test"))

	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 2 {
		t.Fatalf("%d assets against a ceiling of 2", n)
	}

	// The window rolls and the programme may create again. A ceiling that never
	// lifted would be a programme silently frozen at its first busy hour.
	h.now = h.now.Add(opts.Window + time.Minute)
	m.Handle(ctx, frame(t, "five.acme.test"))
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 3 {
		t.Fatalf("%d assets after the window rolled", n)
	}

	// What it held back is still counted, because a SAN under a watched apex is
	// a fact about the logs rather than about what this process created.
	if n := h.count(t, `SELECT san_count FROM ct_apex WHERE program_id = $1`, h.program); n != 0 {
		t.Fatalf("the counters were written before a flush: %d", n)
	}
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := h.count(t, `SELECT san_count FROM ct_apex WHERE program_id = $1`, h.program); n != 5 {
		t.Errorf("the apex counted %d SANs, and the logs delivered five whatever the ceiling did", n)
	}
}

func TestAWildcardMovesTheCountersAndCreatesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m.Handle(ctx, frame(t, "*.acme.test", "*.api.acme.test"))
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 0 {
		t.Fatalf("%d assets from a certificate that reveals no host", n)
	}

	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var sans, wildcards int
	var lastWildcard *time.Time
	err := h.pool.QueryRow(ctx,
		`SELECT san_count, wildcard_count, last_wildcard_at FROM ct_apex WHERE program_id = $1`,
		h.program).Scan(&sans, &wildcards, &lastWildcard)
	if err != nil {
		t.Fatalf("read the apex counters: %v", err)
	}
	if wildcards != 2 || sans != 0 {
		t.Errorf("the apex counted %d SANs and %d wildcards", sans, wildcards)
	}
	if lastWildcard == nil || !lastWildcard.Equal(h.now) {
		t.Errorf("last_wildcard_at is %v", lastWildcard)
	}
}

// Presence rather than absence: a process that dies writes no minute, so a gap
// needs nothing to notice it.
func TestTheFeedRecordsTheMinutesItWasAlive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m.Handle(ctx, frame(t, "unrelated.example.org"))
	m.Handle(ctx, frame(t, "also-unrelated.example.org"))
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var frames int64
	if err := h.pool.QueryRow(ctx,
		`SELECT frames FROM ct_feed_minute WHERE minute = date_trunc('minute', $1::timestamptz)`,
		h.now).Scan(&frames); err != nil {
		t.Fatalf("read the feed minute: %v", err)
	}
	// Frames that matched nothing still count: the record says the socket was
	// alive, not that the perimeter was busy.
	if frames != 2 {
		t.Errorf("the minute recorded %d frames", frames)
	}

	// A minute nothing arrived in writes no row at all, which is the gap.
	h.now = h.now.Add(time.Minute)
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM ct_feed_minute`); n != 1 {
		t.Errorf("%d minutes recorded, and only one of them saw a frame", n)
	}
}

// watched_since would otherwise span a period during which nothing was
// watching, so an apex removed and put back would read as continuously covered.
func TestAnApexLeavingTheSetIsForgotten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	m.Handle(ctx, frame(t, "seen.acme.test"))
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM ct_apex WHERE program_id = $1`, h.program); n != 1 {
		t.Fatalf("%d apex rows after a sighting", n)
	}

	// A rule is closed rather than deleted, which is what the perimeter does.
	h.exec(t, `UPDATE scope_rule SET valid_to = $1 WHERE program_id = $2`, at.Add(-time.Minute), h.program)
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM ct_apex WHERE program_id = $1`, h.program); n != 0 {
		t.Errorf("%d apex rows survive an apex nobody watches", n)
	}

	// And the asset it produced is untouched: the counters are a metric, the
	// inventory is not.
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 1 {
		t.Errorf("forgetting an apex took %d assets with it", 1-n)
	}
}

// An apex watched from now on and silent has to be distinguishable from an apex
// nothing is watching at all, or the coverage reading says the same thing about
// both.
func TestAWatchedApexHasARowBeforeItDeliversAnything(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	var apex string
	var watched time.Time
	var sans int
	err := h.pool.QueryRow(ctx,
		`SELECT apex, watched_since, san_count FROM ct_apex WHERE program_id = $1`,
		h.program).Scan(&apex, &watched, &sans)
	if err != nil {
		t.Fatalf("a watched apex has no row: %v", err)
	}
	if apex != "acme.test" || sans != 0 || !watched.Equal(h.now) {
		t.Errorf("the row says apex %q, %d SANs, watched since %s", apex, sans, watched)
	}

	// A second reload must not move it, or "watched since" would always be now.
	h.now = h.now.Add(time.Hour)
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT watched_since FROM ct_apex WHERE program_id = $1`, h.program).Scan(&watched); err != nil {
		t.Fatalf("read watched_since: %v", err)
	}
	if !watched.Equal(at) {
		t.Errorf("watched_since moved to %s on a reload", watched)
	}
}

func TestACertificateWithNoDNSNameIsHandledAndCountsAsAFrame(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "include", "apex", "acme.test")

	m := h.matcher(options())
	if err := m.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	empty := frame(t)
	empty.Data.LeafCert.AllDomains = nil
	m.Handle(ctx, empty)

	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM asset`); n != 0 {
		t.Errorf("%d assets from a certificate carrying no name", n)
	}
	if n := h.count(t, `SELECT coalesce(sum(frames), 0)::int FROM ct_feed_minute`); n != 1 {
		t.Errorf("%d frames recorded, and one arrived", n)
	}
}
