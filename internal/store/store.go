package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open builds a connection pool and proves it works before returning it.
//
// The role a process connects with is chosen here, when the pool is opened,
// and never case by case in the code. That is what keeps the separation a fact
// of deployment rather than a convention somebody can forget in one query.
func Open(ctx context.Context, url string, maxConns int32, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The message from ParseConfig can contain the string it failed on,
		// which is a connection string with a password in it.
		return nil, fmt.Errorf("parse database url: invalid syntax")
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// A pool is lazy, so without this the first sign of a wrong credential
	// would be a request failing rather than a process refusing to start.
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}
	return pool, nil
}
