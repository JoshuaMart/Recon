//go:build integration

// The half of milestone 5 that only exists at volume.
package notify_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/notify"
)

type fixture struct {
	pool    *pgxpool.Pool
	org     uuid.UUID
	program uuid.UUID
	calls   *atomic.Int64
	fail    *atomic.Bool
	server  *httptest.Server
}

// perimeter builds a programme, a channel and an endpoint that counts what it
// receives.
func perimeter(t *testing.T) *fixture {
	t.Helper()

	pool := newPool(t)
	f := &fixture{
		pool: pool, org: uuid.New(), program: uuid.New(),
		calls: &atomic.Int64{}, fail: &atomic.Bool{},
	}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		f.calls.Add(1)
	}))
	t.Cleanup(f.server.Close)

	exec(t, pool, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, f.org)
	exec(t, pool, `INSERT INTO program (id, org_id, name, authorized_from, created_at)
		VALUES ($1, $2, 'acme', now() - interval '1 day', now() - interval '1 hour')`, f.program, f.org)
	exec(t, pool, `INSERT INTO notification_channel (id, org_id, url, min_priority, managed_by)
		VALUES ($1, $2, $3, 'low', 'config')`, uuid.New(), f.org, f.server.URL)
	return f
}

// arrivals writes n pending events of one kind on their own assets.
func (f *fixture) arrivals(t *testing.T, kind, priority string, n int) {
	t.Helper()

	for i := range n {
		asset := uuid.New()
		exec(t, f.pool, `INSERT INTO asset
			(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,$4,'fastrecon','in_scope', now(), now())`,
			asset, f.org, f.program, fmt.Sprintf("h%05d.acme.test", i))
		exec(t, f.pool, `INSERT INTO notification_event
			(org_id, program_id, asset_id, kind, priority, payload)
			VALUES ($1,$2,$3,$4,$5,'{}'::jsonb)`, f.org, f.program, asset, kind, priority)
	}
}

func (f *fixture) notifier(t *testing.T) *notify.Notifier {
	t.Helper()

	return notify.New(f.pool, notify.NewSender(5*time.Second, nil), 10000, quiet())
}

func (f *fixture) count(t *testing.T, sql string) int {
	t.Helper()

	var n int
	if err := f.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A run turning thousands of assets active sends at most the cap per window,
// the rest carried by summaries, and loses nothing.
func TestAFloodIsCappedSummarisedAndNotLost(t *testing.T) {
	f := perimeter(t)
	ctx := context.Background()

	const flood = 5000
	f.arrivals(t, notify.KindNewActive, notify.High, flood)

	summary, err := f.notifier(t).Once(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	cap := notify.Windows[notify.High].Cap
	if summary.Sent > cap {
		t.Fatalf("%d notifications went out against a cap of %d", summary.Sent, cap)
	}
	if summary.Summarised == 0 {
		t.Fatal("a saturated window produced no summary, which is an anti-flood becoming a silence")
	}

	// Nothing is lost. Every event is either sent or readable and marked.
	sent := f.count(t, `SELECT count(*) FROM notification_event WHERE notified_at IS NOT NULL AND kind = 'new_active'`)
	held := f.count(t, `SELECT count(*) FROM notification_event WHERE suppressed AND kind = 'new_active'`)
	if sent+held != flood {
		t.Fatalf("%d sent and %d held out of %d: %d events are unaccounted for",
			sent, held, flood, flood-sent-held)
	}

	// And the summary carries the priority of the window it replaces, so a
	// channel whose floor is high still receives it.
	var priority string
	if err := f.pool.QueryRow(ctx,
		`SELECT priority FROM notification_event WHERE kind = 'digest' LIMIT 1`).Scan(&priority); err != nil {
		t.Fatalf("read the summary: %v", err)
	}
	if priority != notify.High {
		t.Fatalf("the summary of a high window is %q", priority)
	}
	// One summary per window, not one per suppressed event.
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE kind = 'digest'`); n != 1 {
		t.Fatalf("%d summaries for one saturated window", n)
	}

	// And it stands for what it says it stands for. Written on the first event
	// past the cap it would count one held event and claim to speak for the
	// four thousand nine hundred and seventy nine that followed.
	if held := f.count(t, `SELECT (payload->>'held')::int FROM notification_event WHERE kind = 'digest'`); held != flood-cap {
		t.Fatalf("the summary stands for %d events, and %d were held", held, flood-cap)
	}
}

// A programme going dark must not be swallowed by the summary of twenty new
// assets. A programme event is already an aggregate, and folding it into a
// second one loses it exactly when it matters.
func TestAMassTipIsNotSwallowedByAFlood(t *testing.T) {
	f := perimeter(t)
	ctx := context.Background()

	f.arrivals(t, notify.KindNewActive, notify.High, 200)
	exec(t, f.pool, `INSERT INTO notification_event
		(org_id, program_id, asset_id, kind, priority, payload)
		VALUES ($1,$2,NULL,'program_unobservable','high','{"summary":"gone dark"}'::jsonb)`,
		f.org, f.program)

	if _, err := f.notifier(t).Once(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var notified *time.Time
	var suppressed bool
	if err := f.pool.QueryRow(ctx,
		`SELECT notified_at, suppressed FROM notification_event WHERE kind = 'program_unobservable'`).
		Scan(&notified, &suppressed); err != nil {
		t.Fatalf("read the tip: %v", err)
	}
	if suppressed || notified == nil {
		t.Fatalf("the programme going dark was suppressed=%v notified=%v behind a flood of arrivals",
			suppressed, notified)
	}
}

// notified_at is set only on a 2xx, and that is the one rule stopping a webhook
// outage from becoming a silent loss of alerts.
func TestAWebhookOutageLosesNothingAndResumes(t *testing.T) {
	f := perimeter(t)
	ctx := context.Background()

	f.arrivals(t, notify.KindTakeover, notify.Critical, 3)
	f.fail.Store(true)

	summary, err := f.notifier(t).Once(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if summary.Sent != 0 || summary.Failed == 0 {
		t.Fatalf("a failing webhook read as %+v", summary)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE notified_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d events were marked notified against a webhook answering 500", n)
	}
	// And they are still in the queue rather than suppressed, which is what
	// makes the stuck queue alert the thing that reports the outage.
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE notified_at IS NULL AND NOT suppressed`); n != 3 {
		t.Fatalf("%d events are still queued", n)
	}

	f.fail.Store(false)
	if _, err := f.notifier(t).Once(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE notified_at IS NOT NULL`); n != 3 {
		t.Fatalf("%d events went out once the webhook recovered", n)
	}
}

// A grace nothing ends expires at seven days, emits its summary, and reports
// the incident. A programme whose first run does not finish in a week is one,
// and absorbing it in silence is a perimeter quietly ceasing to be scanned.
func TestAGraceNothingEndsExpiresAndReportsTheIncident(t *testing.T) {
	f := perimeter(t)
	ctx := context.Background()

	exec(t, f.pool, `UPDATE program SET created_at = now() - interval '8 days' WHERE id = $1`, f.program)
	exec(t, f.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'expired', now() - interval '7 days')`,
		f.org, f.program)
	f.arrivals(t, notify.KindNewActive, notify.High, 12)
	exec(t, f.pool, `UPDATE notification_event SET suppressed = true WHERE kind = 'new_active'`)

	if err := f.notifier(t).Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}

	var held int
	var incident *string
	if err := f.pool.QueryRow(ctx,
		`SELECT (payload->>'held')::int, payload->>'incident'
		   FROM notification_event WHERE kind = 'digest'`).Scan(&held, &incident); err != nil {
		t.Fatalf("read the summary: %v", err)
	}
	if held != 12 {
		t.Fatalf("the summary stands for %d events", held)
	}
	if incident == nil {
		t.Fatal("a first run that never completed was absorbed in silence")
	}
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE kind = 'run_never_completed'`); n != 1 {
		t.Fatalf("%d incidents reported", n)
	}
}

// A failed first run leaves the grace active; a later successful one ends it.
// A "grace consumed" column would force deciding whether a failure consumes it,
// and that question has no good answer.
func TestAFailedFirstRunLeavesTheGraceAndASuccessEndsIt(t *testing.T) {
	f := perimeter(t)
	ctx := context.Background()

	exec(t, f.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'expired', now())`, f.org, f.program)
	f.arrivals(t, notify.KindNewActive, notify.High, 8)
	exec(t, f.pool, `UPDATE notification_event SET suppressed = true WHERE kind = 'new_active'`)

	if err := f.notifier(t).Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE kind = 'digest'`); n != 0 {
		t.Fatalf("%d summaries after a failed first run, and the grace should still hold", n)
	}

	exec(t, f.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, finished_at)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'completed', now(), now())`, f.org, f.program)
	if err := f.notifier(t).Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_event WHERE kind = 'digest'`); n != 1 {
		t.Fatalf("%d summaries after the run completed", n)
	}
}
