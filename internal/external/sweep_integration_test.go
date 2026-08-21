//go:build integration

// The two halves of external_host_dead that only a database can answer.
package external_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/external"
	"github.com/JoshuaMart/recon/internal/store"
)

// answers is a resolver that knows what a test decided.
//
// The alternative is a test that depends on somebody's expired domain staying
// expired, which goes red on a Tuesday for a reason nobody can act on.
type answers struct {
	gone  map[string]bool
	fails map[string]bool
	asked []string
}

func (a *answers) Registered(_ context.Context, apex string) (bool, error) {
	a.asked = append(a.asked, apex)
	if a.fails[apex] {
		return true, context.DeadlineExceeded
	}
	return !a.gone[apex], nil
}

type harness struct {
	pool    *pgxpool.Pool
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

	h := &harness{pool: pool, org: uuid.New(), program: uuid.New()}
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, h.org)
	h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'p', now())`,
		h.program, h.org)
	return h
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (h *harness) asset(t *testing.T, key, lifecycle string, attributes string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	kind := "fqdn"
	host := key
	if strings.Contains(key, ":") {
		kind = "service"
		host, _, _ = strings.Cut(key, ":")
	}
	h.exec(t, `INSERT INTO asset (id, org_id, program_id, kind, key, host, discovery_source,
	                              scope_status, first_seen, last_seen)
	           VALUES ($1,$2,$3,$4,$5,$6,'manual','in_scope',now(),now())`,
		id, h.org, h.program, kind, key, host)
	h.exec(t, `INSERT INTO asset_current (asset_id, org_id, program_id, kind, key, host,
	                                      scope_status, lifecycle, attributes, first_seen, last_seen)
	           VALUES ($1,$2,$3,$4,$5,$6,'in_scope',$7,$8::jsonb,now(),now())`,
		id, h.org, h.program, kind, key, host, lifecycle, attributes)
	return id
}

func (h *harness) events(t *testing.T) []string {
	t.Helper()

	rows, err := h.pool.Query(context.Background(),
		`SELECT payload ->> 'external_host' FROM notification_event
		  WHERE kind = 'external_host_dead' ORDER BY id`)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, host)
	}
	return out
}

func sweep(t *testing.T, h *harness, resolver external.Resolver) *external.Sweep {
	t.Helper()

	return external.New(h.pool, resolver, time.Hour, 100,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestAnExpiredThirdPartyDomainIsFoundAndToldOnce.
func TestAnExpiredThirdPartyDomainIsFoundAndToldOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.asset(t, "app.acme.test:443/tcp", "active",
		`{"external_hosts":["cdn.expired.test","fonts.alive.test"]}`)

	resolver := &answers{gone: map[string]bool{"expired.test": true}}
	if err := sweep(t, h, resolver).Once(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	found := h.events(t)
	if len(found) != 1 || found[0] != "cdn.expired.test" {
		t.Fatalf("the sweep produced %v, want the expired host alone", found)
	}

	// The apex and only the apex. Resolving the host itself would answer a
	// different question: a name that no longer resolves under a domain that is
	// still registered is a dangling subdomain at somebody else's, which is not
	// re-registrable and not this finding.
	for _, asked := range resolver.asked {
		if strings.Count(asked, ".") != 1 {
			t.Errorf("the sweep resolved %q, which is not an apex", asked)
		}
	}

	// A second tick finds the same domain still gone and says nothing. The
	// verdict lives on the asset, so the emission is on the transition without
	// a second mechanism for it.
	if err := sweep(t, h, resolver).Once(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again := h.events(t); len(again) != 1 {
		t.Errorf("a second tick produced %d events, want the first one alone", len(again))
	}

	// And a domain somebody re-registered stops being reported, which is why an
	// empty list is written rather than skipped.
	resolver.gone = map[string]bool{}
	if err := sweep(t, h, resolver).Once(ctx); err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	var dead int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM asset_current WHERE attributes ? 'dead_external_hosts'`).Scan(&dead); err != nil {
		t.Fatalf("read: %v", err)
	}
	if dead != 0 {
		t.Error("a re-registered domain is still marked dead on the asset")
	}
}

// TestAHostInTheSameInventoryIsTheOtherHalvesBusiness.
//
// It has a lifecycle of its own, so resolving it would be asking a resolver a
// question this system already measures, and it would answer differently.
func TestAHostInTheSameInventoryIsTheOtherHalvesBusiness(t *testing.T) {
	h := newHarness(t)

	h.asset(t, "static.acme.test", "inactive", `{}`)
	h.asset(t, "app.acme.test:443/tcp", "active", `{"external_hosts":["static.acme.test"]}`)

	resolver := &answers{gone: map[string]bool{"acme.test": true}}
	if err := sweep(t, h, resolver).Once(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(resolver.asked) != 0 {
		t.Errorf("the sweep resolved %v, and those hosts are in the inventory", resolver.asked)
	}
	if found := h.events(t); len(found) != 0 {
		t.Errorf("the sweep spoke for a host the internal half owns: %v", found)
	}
}

// TestAResolverHavingABadMinuteIsNotAnExpiry, in both directions.
//
// A timeout, a refusal or a broken resolver are not an authoritative "no such
// domain", so one must not create a finding. And it is not an authoritative
// "still registered" either, so one must not erase a finding already recorded:
// dropping the entry and re-adding it on the next successful tick reads as a
// new finding, and the same critical alert goes out again for a host that has
// been dead all week.
func TestAResolverHavingABadMinuteIsNotAnExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.asset(t, "app.acme.test:443/tcp", "active",
		`{"external_hosts":["cdn.unreachable.test","cdn.expired.test"]}`)

	// It creates nothing.
	broken := &answers{fails: map[string]bool{"unreachable.test": true, "expired.test": true}}
	if err := sweep(t, h, broken).Once(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if found := h.events(t); len(found) != 0 {
		t.Fatalf("a resolver failure produced %v", found)
	}

	// A tick that works records the one that is really gone.
	working := &answers{gone: map[string]bool{"expired.test": true}}
	if err := sweep(t, h, working).Once(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if found := h.events(t); len(found) != 1 {
		t.Fatalf("the working tick produced %v, want one finding", found)
	}

	// And a tick where the resolver stumbles on that same apex leaves the
	// finding exactly where it was.
	stumbling := &answers{fails: map[string]bool{"expired.test": true, "unreachable.test": true}}
	if err := sweep(t, h, stumbling).Once(ctx); err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	var recorded int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM asset_current
		  WHERE attributes -> 'dead_external_hosts' @> '["cdn.expired.test"]'::jsonb`).Scan(&recorded); err != nil {
		t.Fatalf("read: %v", err)
	}
	if recorded != 1 {
		t.Error("a failed lookup erased a finding the previous tick had recorded")
	}

	// Which is what stops the next working tick re-sending it.
	if err := sweep(t, h, working).Once(ctx); err != nil {
		t.Fatalf("fourth sweep: %v", err)
	}
	if found := h.events(t); len(found) != 1 {
		t.Errorf("%d findings after a resolver stumbled and recovered, want the first one alone",
			len(found))
	}
}

// TestTheWalkGoesPastOnePage.
//
// A fixed cap with a fixed order sweeps the same lowest identifiers on every
// tick, so anything past it is permanently invisible: an expired domain
// referenced only by a high identifier is never resolved and never told, and
// nothing anywhere says so.
func TestTheWalkGoesPastOnePage(t *testing.T) {
	h := newHarness(t)

	for i := range 12 {
		h.asset(t, fmt.Sprintf("h%02d.acme.test:443/tcp", i), "active",
			`{"external_hosts":["cdn.expired.test"]}`)
	}

	// A page smaller than the set, which is the whole point.
	tick := external.New(h.pool, &answers{gone: map[string]bool{"expired.test": true}},
		time.Hour, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := tick.Once(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if found := h.events(t); len(found) != 12 {
		t.Errorf("%d findings for 12 referencing assets, want all of them: the walk stopped at a page",
			len(found))
	}
}

// A verdict that says what it already said is not written.
//
// In steady state that is every asset carrying an external host, so writing
// them all would be thousands of transactions and thousands of dead row
// versions every tick, to store what was already there.
func TestASettledVerdictIsNotRewritten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.asset(t, "app.acme.test:443/tcp", "active",
		`{"external_hosts":["cdn.expired.test","fonts.alive.test"]}`)
	resolver := &answers{gone: map[string]bool{"expired.test": true}}
	if err := sweep(t, h, resolver).Once(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var before int
	if err := h.pool.QueryRow(ctx,
		`SELECT xmin::text::bigint FROM asset_current WHERE asset_id = $1`, id).Scan(&before); err != nil {
		t.Fatalf("read the row version: %v", err)
	}

	for range 3 {
		if err := sweep(t, h, resolver).Once(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}

	var after int
	if err := h.pool.QueryRow(ctx,
		`SELECT xmin::text::bigint FROM asset_current WHERE asset_id = $1`, id).Scan(&after); err != nil {
		t.Fatalf("read the row version: %v", err)
	}
	if after != before {
		t.Errorf("three ticks that concluded nothing new rewrote the row (%d then %d)", before, after)
	}
}

// An archived asset is not a lead, so nothing is resolved on its behalf.
func TestAnArchivedAssetIsNotFollowed(t *testing.T) {
	h := newHarness(t)

	h.asset(t, "gone.acme.test:443/tcp", "archived", `{"external_hosts":["cdn.expired.test"]}`)

	resolver := &answers{gone: map[string]bool{"expired.test": true}}
	if err := sweep(t, h, resolver).Once(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if found := h.events(t); len(found) != 0 {
		t.Errorf("an archived asset produced %v", found)
	}
}

// The apex is read through the public suffix list, because "the last two
// labels" is right for example.com and wrong for example.co.uk, and resolving a
// public suffix always succeeds, so every finding under it would disappear.
func TestTheApexIsReadThroughThePublicSuffixList(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]string{
		"cdn.example.com":          "example.com",
		"a.b.c.example.co.uk":      "example.co.uk",
		"assets.example.github.io": "example.github.io",
		"example.com":              "example.com",
		"":                         "",
	} {
		if got := external.Apex(name); got != want {
			t.Errorf("Apex(%q) = %q, want %q", name, got, want)
		}
	}
}
