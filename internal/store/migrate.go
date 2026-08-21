// Package store owns the database connections and the schema.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/JoshuaMart/recon/db"
)

// Direction is what a migration run does.
type Direction string

const (
	// Up applies every pending migration.
	Up Direction = "up"
	// Down rolls back exactly one, which is what makes reversibility
	// testable one step at a time.
	Down Direction = "down"
	// Reset rolls back everything, for the test that proves a schema can be
	// unwound and replayed without loss.
	Reset Direction = "reset"
)

// Migrator applies the embedded migrations.
type Migrator struct {
	provider *goose.Provider
	db       *sql.DB
	log      *slog.Logger
}

// NewMigrator opens a connection as the owner and prepares the runner.
//
// The caller closes it. A session-level advisory lock is held for the duration
// of a run, so that two instances starting at once never migrate in parallel:
// the second waits and then finds nothing to do, rather than applying the same
// statement twice.
func NewMigrator(url string, log *slog.Logger) (*Migrator, error) {
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	conn, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// The advisory lock is session-scoped, so it has to be taken and released
	// on the same connection. More than one would let the lock be held by a
	// session that the pool hands to somebody else.
	conn.SetMaxOpenConns(1)

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("build session locker: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, conn, migrations,
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("build migration provider: %w", err)
	}

	return &Migrator{provider: provider, db: conn, log: log}, nil
}

// Close releases the connection.
func (m *Migrator) Close() error { return m.db.Close() }

// Run applies migrations in one direction and reports what moved.
func (m *Migrator) Run(ctx context.Context, direction Direction) error {
	switch direction {
	case Up:
		results, err := m.provider.Up(ctx)
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		m.report(ctx, results)

	case Down:
		result, err := m.provider.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		m.report(ctx, []*goose.MigrationResult{result})

	case Reset:
		results, err := m.provider.DownTo(ctx, 0)
		if err != nil {
			return fmt.Errorf("migrate reset: %w", err)
		}
		m.report(ctx, results)

	default:
		return fmt.Errorf("unknown direction %q", direction)
	}
	return nil
}

// Version is the migration the database currently sits on.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	version, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Status lists every migration with whether it has been applied.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	status, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read migration status: %w", err)
	}
	return status, nil
}

func (m *Migrator) report(ctx context.Context, results []*goose.MigrationResult) {
	if len(results) == 0 {
		m.log.InfoContext(ctx, "schema already up to date")
		return
	}
	for _, r := range results {
		if r == nil {
			continue
		}
		m.log.InfoContext(ctx, "migration applied",
			"version", r.Source.Version,
			"name", r.Source.Path,
			"direction", r.Direction,
			"duration", r.Duration.String(),
		)
	}
}

// register keeps the pgx driver linked in. The import above is what registers
// it with database/sql, and a linter that removes unused imports would
// otherwise take the driver with it.
var _ = stdlib.GetDefaultDriver
