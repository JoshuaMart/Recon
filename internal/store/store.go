package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open builds a connection pool and proves it works before returning it. The role
// a process connects with is chosen here, when the pool opens, never case by case,
// keeping the separation a fact of deployment and not a convention one query can
// forget.
func Open(ctx context.Context, url string, maxConns int32, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// ParseConfig's message can contain the string it failed on: a connection string
		// with a password.
		return nil, fmt.Errorf("parse database url: invalid syntax")
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// A pool is lazy; without this a wrong credential shows as a failing request, not a
	// process refusing to start.
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}
	return pool, nil
}
