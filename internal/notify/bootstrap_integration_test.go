//go:build integration

// The configured channel, and the one thing about it that is easy to get wrong.
package notify_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func newPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The row is keyed on the organization and the config marker rather than on the
// URL. Without that marker, changing the configured URL and restarting inserts
// a second active row without disabling the first, and every alert then goes
// out twice, one of them to the destination just replaced.
func TestChangingTheConfiguredURLDoesNotDoubleTheAlerts(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	org := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, org); err != nil {
		t.Fatalf("org: %v", err)
	}

	for _, url := range []string{"https://hooks.example/one", "https://hooks.example/two"} {
		settled, err := notify.Bootstrap(ctx, q, url, "", notify.Low, quiet())
		if err != nil || !settled {
			t.Fatalf("bootstrap %s: settled=%v %v", url, settled, err)
		}
	}

	rows, err := q.ChannelsForOrg(ctx, sqlcgen.ChannelsForOrgParams{OrgID: pgUUID(org)})
	if err != nil {
		t.Fatalf("read channels: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d enabled channels after one URL change, and every alert would go to each", len(rows))
	}
	if rows[0].Url != "https://hooks.example/two" {
		t.Fatalf("the channel points at %q", rows[0].Url)
	}
}

// The organization is created by a command that runs outside this process, so a
// bootstrap that only ever ran at startup would leave the channel missing until
// somebody restarted for an unrelated reason.
func TestTheChannelWaitsForATenantAndThenSettles(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	settled, err := notify.Bootstrap(ctx, q, "https://hooks.example/one", "", notify.Low, quiet())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if settled {
		t.Fatal("the bootstrap gave up before any organization existed")
	}

	org := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, org); err != nil {
		t.Fatalf("org: %v", err)
	}
	if settled, err = notify.Bootstrap(ctx, q, "https://hooks.example/one", "", notify.Low, quiet()); err != nil || !settled {
		t.Fatalf("the retry did not settle: %v %v", settled, err)
	}

	// And a second tenant stops it applying, because a configuration file has
	// no way to name one.
	if _, err := pool.Exec(ctx, `INSERT INTO org (id, name) VALUES ($1, 'other')`, uuid.New()); err != nil {
		t.Fatalf("org: %v", err)
	}
	rows, err := q.ChannelsForOrg(ctx, sqlcgen.ChannelsForOrgParams{OrgID: pgUUID(org)})
	if err != nil {
		t.Fatalf("read channels: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d channels", len(rows))
	}
}

// The grace holds back the flood, and holding it back without saying so is the
// same loss of signal the cap exists to avoid. A first run that notifies
// nothing at all is an anti-flood that became a silence.
func TestAFirstRunIsSummarisedRatherThanSilent(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	org, program := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, org)
	exec(t, pool, `INSERT INTO program (id, org_id, name, authorized_from, created_at)
		VALUES ($1, $2, 'acme', now() - interval '1 day', now() - interval '1 hour')`, program, org)
	// A discovery run in flight: the grace holds.
	exec(t, pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, apex)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'running', now() + interval '1 hour', 'acme.test')`,
		org, program)
	for n := range 40 {
		asset := uuid.New()
		exec(t, pool, `INSERT INTO asset
			(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,$4,'fastrecon','in_scope', now(), now())`,
			asset, org, program, fmt.Sprintf("h%02d.acme.test", n))
		exec(t, pool, `INSERT INTO notification_event (org_id, program_id, asset_id, kind, priority, payload, suppressed)
			VALUES ($1, $2, $3, 'new_active', 'high', '{}'::jsonb, true)`, org, program, asset)
	}

	notifier := notify.New(pool, notify.NewSender(time.Second, nil), 50, quiet())

	// While the run is going, nothing is summarised: a summary written during
	// the run carries a count already wrong by the time it lands.
	if err := notifier.Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM notification_event WHERE kind = 'digest' AND NOT suppressed`); n != 0 {
		t.Fatalf("%d summaries were emitted during the first run", n)
	}

	exec(t, pool, `UPDATE run SET state = 'completed', finished_at = now() WHERE program_id = $1`, program)
	if err := notifier.Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}

	var summary string
	var assetID *string
	if err := pool.QueryRow(ctx,
		`SELECT payload->>'summary', asset_id::text FROM notification_event
		  WHERE kind = 'digest' AND NOT suppressed`).Scan(&summary, &assetID); err != nil {
		t.Fatalf("read the summary: %v", err)
	}
	if assetID != nil {
		t.Fatalf("the summary claims to designate asset %s", *assetID)
	}

	// And it is emitted once. A second tick must not repeat it.
	if err := notifier.Aggregates(ctx, 0.5); err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM notification_event WHERE kind = 'digest' AND NOT suppressed`); n != 1 {
		t.Fatalf("%d summaries after two ticks", n)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
