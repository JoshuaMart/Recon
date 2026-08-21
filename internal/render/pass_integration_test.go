//go:build integration

// The render pass end to end: what is selected, what is charged, what is
// written, and what a saturated service does to all of it.
package render_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/render"
	"github.com/JoshuaMart/recon/internal/store"
)

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
	h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from, rate_limit_rps)
	           VALUES ($1, $2, 'p', now() - interval '1 day', 1000)`, h.program, h.org)
	return h
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// service writes one due render, with the priority it should be served at.
func (h *harness) service(t *testing.T, key string, port int, priority int16, due time.Time) uuid.UUID {
	t.Helper()

	id := uuid.New()
	h.exec(t, `INSERT INTO asset
		(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1,$2,$3,'service',$4,'app.acme.test','fixture','in_scope', now(), now())`,
		id, h.org, h.program, key)
	h.exec(t, `INSERT INTO asset_current
		(asset_id, org_id, program_id, kind, key, scope_status, host, port, scheme,
		 lifecycle, next_fingerprint_at, fingerprint_priority, first_seen, last_seen)
		VALUES ($1,$2,$3,'service',$4,'in_scope','app.acme.test',$5,'https',
		        'active',$6,$7, now(), now())`,
		id, h.org, h.program, key, port, due, priority)
	return id
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func permissive() *fingerprint.Guard {
	return fingerprint.NewGuard(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
}

// The order is the assertion. A first scan makes thousands of baselines due at
// the same instant, and a render triggered by a detected change five minutes
// later carries a later date: ordered on the date alone it would sort behind
// every one of them.
func TestPriorityIsServedBeforeTheDueDate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	old := time.Now().Add(-2 * time.Hour)
	h.service(t, "app.acme.test:8081/tcp", 8081, lifecycle.PriorityBaseline, old)
	h.service(t, "app.acme.test:8082/tcp", 8082, lifecycle.PriorityBaseline, old)
	// Due later, and urgent.
	h.service(t, "app.acme.test:8443/tcp", 8443, lifecycle.PriorityChange, time.Now().Add(-time.Minute))

	var order []string
	var mu atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if mu.Add(1) == 1 {
			order = append(order, body.URL)
		}
		writeRender(w, body.URL)
	}))
	defer server.Close()

	pass := render.New(h.pool,
		fingerprint.New(server.URL, 5*time.Second, permissive()),
		ingest.New(nil, lifecycle.DefaultCadence(), quiet()),
		render.NewBudget(30, nil),
		// One at a time, so the order the queue produced is the order observed.
		render.Options{Batch: 10, Concurrency: 1},
		quiet())

	summary, err := pass.Once(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if summary.Selected != 3 || summary.Rendered != 3 {
		t.Fatalf("the pass reads %+v", summary)
	}
	if len(order) == 0 || order[0] != "https://app.acme.test:8443/" {
		t.Fatalf("the first render was %v, and the urgent one carries the later due date", order)
	}
}

// A render that happened moves the asset out to its regime's cadence and back
// down to the low queue. Leaving the priority raised would keep an asset that
// was urgent once ahead of the queue for every pass afterwards.
func TestARenderRescheduleItselfAndGivesUpItsPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.service(t, "app.acme.test:443/tcp", 443, lifecycle.PriorityChange, time.Now().Add(-time.Hour))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeRender(w, body.URL)
	}))
	defer server.Close()

	pass := render.New(h.pool,
		fingerprint.New(server.URL, 5*time.Second, permissive()),
		ingest.New(nil, lifecycle.DefaultCadence(), quiet()),
		render.NewBudget(30, nil), render.Options{Batch: 10, Concurrency: 1}, quiet())

	if _, err := pass.Once(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}

	var due time.Time
	var priority int16
	if err := h.pool.QueryRow(ctx,
		`SELECT next_fingerprint_at, fingerprint_priority FROM asset_current WHERE asset_id = $1`,
		id).Scan(&due, &priority); err != nil {
		t.Fatalf("read the schedule: %v", err)
	}
	if priority != lifecycle.PriorityBaseline {
		t.Errorf("the asset kept priority %d after its render", priority)
	}
	if !due.After(time.Now().Add(20 * 24 * time.Hour)) {
		t.Errorf("the next render is at %s, and the nominal cadence is three weeks", due)
	}

	// A second pass finds nothing: the queue is a predicate, and the predicate
	// is now false for this asset.
	summary, err := pass.Once(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if summary.Selected != 0 {
		t.Fatalf("%d assets were still due after being rendered", summary.Selected)
	}
}

// A 429 is a state of the service, so it must not touch the asset: no
// observation, no counter, no timestamp, and no due date moved. Nothing is
// lost, and the reason is structural rather than a retry.
func TestASaturatedServiceLosesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.service(t, "app.acme.test:443/tcp", 443, lifecycle.PriorityBaseline, time.Now().Add(-time.Hour))

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	pass := render.New(h.pool,
		fingerprint.New(server.URL, 5*time.Second, permissive()),
		ingest.New(nil, lifecycle.DefaultCadence(), quiet()),
		render.NewBudget(30, nil),
		render.Options{
			Batch: 10, Concurrency: 1,
			// The wait is honoured by the pass, not by the clock of a test.
			Sleep: func(context.Context, time.Duration) bool { return true },
		}, quiet())

	summary, err := pass.Once(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if !summary.Saturated || summary.Rendered != 0 {
		t.Fatalf("the pass reads %+v", summary)
	}

	var due time.Time
	var streak int
	var last *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT next_fingerprint_at, fingerprint_streak, last_fingerprint_at
		   FROM asset_current WHERE asset_id = $1`, id).Scan(&due, &streak, &last); err != nil {
		t.Fatalf("read the asset: %v", err)
	}
	if streak != 0 || last != nil {
		t.Fatalf("a refusal touched the asset: streak %d, last render %v", streak, last)
	}
	if !due.Before(time.Now()) {
		t.Fatalf("the due date moved to %s, and the due date is the only queue there is", due)
	}
	if n := count(t, h.pool, `SELECT count(*) FROM observation WHERE org_id = $1`, h.org); n != 0 {
		t.Fatalf("%d observations from a service that rendered nothing", n)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func writeRender(w http.ResponseWriter, url string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fingerprint.Result{
		URL:     url,
		Version: "2.1.0",
		Chain: []fingerprint.Hop{{
			URL: url, StatusCode: 200, Title: "App",
			Headers: map[string]string{"Server": "nginx"},
		}},
	})
}

// A mass tip into unobservable is a different event from one asset going quiet,
// and it usually says something about the observer rather than about the
// targets: an address that got banned, an egress that broke, a renderer that
// stopped clearing challenges. Swallowed by a per asset view it is invisible
// exactly when it matters.
func TestAProgrammeTippingWholesaleIsAnAlert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// One asset to make the pass do something, and nine to be counted.
	h.service(t, "app.acme.test:443/tcp", 443, lifecycle.PriorityBaseline, time.Now().Add(-time.Hour))
	for port := 8000; port < 8009; port++ {
		h.service(t, key(port), port, lifecycle.PriorityBaseline, time.Now().Add(24*time.Hour))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeRender(w, body.URL)
	}))
	defer server.Close()

	records := &recorder{}
	build := func() *render.Pass {
		return render.New(h.pool,
			fingerprint.New(server.URL, 5*time.Second, permissive()),
			ingest.New(nil, lifecycle.DefaultCadence(), quiet()),
			render.NewBudget(30, nil),
			render.Options{Batch: 10, Concurrency: 1, UnobservableAlert: 0.2},
			slog.New(records))
	}

	// One in ten is below the threshold. A single asset nobody can reach is an
	// asset, not an incident.
	h.exec(t, `UPDATE asset_current SET lifecycle = 'unobservable' WHERE key = $1`, key(8000))
	if _, err := build().Once(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if records.alerted() {
		t.Fatal("one asset in ten raised the alert that exists for a programme tipping")
	}

	// Three in ten is not.
	h.exec(t, `UPDATE asset_current SET lifecycle = 'unobservable' WHERE key = ANY($1)`,
		[]string{key(8001), key(8002)})
	// A fresh pass, because the census is throttled inside one.
	records.reset()
	h.exec(t, `UPDATE asset_current SET next_fingerprint_at = now() - interval '1 hour'
	           WHERE key = 'app.acme.test:443/tcp'`)
	if _, err := build().Once(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if !records.alerted() {
		t.Fatal("a programme with three assets in ten unobservable raised nothing")
	}
}

func key(port int) string { return "app.acme.test:" + strconv.Itoa(port) + "/tcp" }

// recorder keeps what was logged, so an alert can be asserted rather than read.
type recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

func (r *recorder) alerted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.Level >= slog.LevelError && strings.Contains(record.Message, "unobservable") {
			return true
		}
	}
	return false
}
