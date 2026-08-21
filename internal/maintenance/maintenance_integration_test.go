//go:build integration

// Milestone 1: monthly partitions are created automatically.
//
// "Automatically" is the word being tested. The function has existed since the
// schema migration and a test has called it by hand since then; what this
// asserts is that something calls it without being asked.
package maintenance_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/maintenance"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

func start(t *testing.T) *pgxpool.Pool {
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

func partitions(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_inherits WHERE inhparent = 'observation'::regclass`).Scan(&n); err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	return n
}

func TestTheLoopCreatesTheMonthsAheadWithoutBeingAsked(t *testing.T) {
	ctx := context.Background()
	pool := start(t)

	// Drop everything but the current month, which is what a database that
	// has been running since before this loop existed looks like.
	rows, err := pool.Query(ctx, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'observation'::regclass
		   AND c.relname <> 'observation_' || to_char(CURRENT_DATE, 'YYYY_MM')`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	var doomed []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		doomed = append(doomed, name)
	}
	rows.Close()

	for _, name := range doomed {
		if _, err := pool.Exec(ctx, `DROP TABLE `+name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
	if got := partitions(t, pool); got != 1 {
		t.Fatalf("%d partitions left, want only the current month", got)
	}

	// A short interval, and Run does a pass immediately rather than waiting
	// for the first tick: a deployment starting on the last day of a month
	// must not depend on staying up for an hour.
	loop := maintenance.New(sqlcgen.New(pool), 50*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	running, stop := context.WithCancel(ctx)
	go loop.Run(running)
	defer stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if partitions(t, pool) >= 3 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Errorf("%d partitions after the loop ran, want the current month and the two ahead: "+
		"two rather than one so an incident at the end of a month does not interrupt ingestion",
		partitions(t, pool))
}
