package runs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Cadence is the pass that provisions enumeration.
//
// It is separate from the due-date pass and complementary to it: this one gives
// regular coverage, and the console endpoint re-runs a perimeter after a scope
// change without waiting for the next tick.
type Cadence struct {
	pool      *pgxpool.Pool
	scheduler *Scheduler
	interval  time.Duration
	log       *slog.Logger
}

// NewCadence builds the loop.
func NewCadence(pool *pgxpool.Pool, scheduler *Scheduler, interval time.Duration, log *slog.Logger) *Cadence {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Cadence{pool: pool, scheduler: scheduler, interval: interval, log: log}
}

// Run ticks until the context ends.
func (c *Cadence) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.Once(ctx); err != nil {
				c.log.ErrorContext(ctx, "discovery cadence failed", "error", err)
			}
		}
	}
}

// Once provisions one discovery run per programme that is due.
//
// The absence of a run in flight is the condition that does the work. It
// prevents a provisioning storm, because last_discovery_at is written when the
// run is created rather than when it completes, and it bounds concurrency to
// one discovery run per programme.
func (c *Cadence) Once(ctx context.Context) (int, error) {
	queries := sqlcgen.New(c.pool)

	due, err := queries.ProgramsDueForDiscovery(ctx, sqlcgen.ProgramsDueForDiscoveryParams{
		At:    stamp(c.scheduler.now()),
		Retry: interval(c.scheduler.cfg.DiscoveryRetry),
	})
	if err != nil {
		return 0, err
	}

	started := 0
	for _, program := range due {
		if ctx.Err() != nil {
			return started, ctx.Err()
		}
		if c.provision(ctx, uuid.UUID(program.OrgID.Bytes), uuid.UUID(program.ID.Bytes), program.Name) {
			started++
		}
	}
	return started, nil
}

// provision defines and starts one run, and reports whether it went out.
func (c *Cadence) provision(ctx context.Context, org, program uuid.UUID, name string) bool {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		c.log.ErrorContext(ctx, "begin failed", "program", program, "error", err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	definition, err := c.scheduler.Discovery(ctx, sqlcgen.New(tx), org, program)
	switch {
	case errors.Is(err, ErrNoPerimeter):
		// The selection above already requires an apex, so reaching this means
		// the rule was closed between the two statements. Rare, and worth one
		// line rather than the warning a minute it used to be.
		c.log.WarnContext(ctx, "programme declares no apex", "program", program, "name", name)
		return false
	case errors.Is(err, ErrRunInFlight):
		return false
	case err != nil:
		c.log.WarnContext(ctx, "discovery refused", "program", program, "name", name, "error", err)
		return false
	}

	if err := tx.Commit(ctx); err != nil {
		c.log.ErrorContext(ctx, "commit failed", "program", program, "error", err)
		return false
	}

	// After the commit, deliberately. A platform that refuses on a quota leaves
	// a row the deadline sweeper owns, and the next tick tries again once that
	// row is out of the way.
	// The cadence serves every tenant, so its pool crosses them and the record
	// below needs no organization of its own.
	record := func(ctx context.Context, runID uuid.UUID, external string) error {
		return sqlcgen.New(c.pool).RecordRunStart(ctx, sqlcgen.RecordRunStartParams{
			RunID: pgUUID(runID), ExternalID: external,
		})
	}
	if err := c.scheduler.Launch(ctx, record, definition); err != nil {
		c.log.ErrorContext(ctx, "discovery run not started",
			"program", program, "name", name, "run", definition.RunID, "error", err)
		return false
	}
	return true
}
