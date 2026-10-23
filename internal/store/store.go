// Package store owns the PostgreSQL connection pool, the migrations and the
// generated queries.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/JoshuaMart/recon/db"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Store is the pool and the queries bound to it.
type Store struct {
	Pool *pgxpool.Pool
	Q    *sqlcgen.Queries
}

// Open connects and checks the connection answers.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping: %w", err)
	}
	return &Store{Pool: pool, Q: sqlcgen.New(pool)}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrator returns a goose provider over the embedded migrations. A session
// advisory lock keeps two processes from migrating at once.
func (s *Store) Migrator() (*goose.Provider, *sql.DB, error) {
	return NewMigrator(stdlib.OpenDBFromPool(s.Pool))
}

// NewMigrator builds the provider on an existing database/sql handle.
func NewMigrator(sqlDB *sql.DB) (*goose.Provider, *sql.DB, error) {
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return nil, nil, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, nil, fmt.Errorf("migrations: %w", err)
	}
	return p, sqlDB, nil
}

// MigrateUp applies every pending migration and returns the resulting version.
func (s *Store) MigrateUp(ctx context.Context) (int64, error) {
	p, sqlDB, err := s.Migrator()
	if err != nil {
		return 0, err
	}
	defer sqlDB.Close()
	if _, err := p.Up(ctx); err != nil {
		return 0, fmt.Errorf("migrate up: %w", err)
	}
	return p.GetDBVersion(ctx)
}
