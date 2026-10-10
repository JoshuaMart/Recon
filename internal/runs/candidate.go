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

// CandidatePass is the third pass, existing for what it does not wait for. A CT candidate is
// due one minute after creation and the aggressive curve rests on that check, but a live
// verification run holds its programme's slot for the whole deadline, so a candidate arriving
// a minute into a sweep would wait half an hour. The passes also exclude each other in the
// selection, or they fight over the same names.
type CandidatePass struct {
	pool      *pgxpool.Pool
	scheduler *Scheduler
	interval  time.Duration
	log       *slog.Logger
}

// NewCandidatePass builds the loop.
func NewCandidatePass(
	pool *pgxpool.Pool, scheduler *Scheduler, interval time.Duration, log *slog.Logger,
) *CandidatePass {
	if interval <= 0 {
		interval = time.Minute
	}
	return &CandidatePass{pool: pool, scheduler: scheduler, interval: interval, log: log}
}

// Run ticks until the context ends. The interval is the floor under the curve's
// first rung (a one-minute rung checked every five is a five), so it shares the
// due-date pass's interval. Due dates and the one live candidate run per
// programme bound the executions, so an empty tick starts nothing.
func (c *CandidatePass) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.Once(ctx); err != nil {
				c.log.ErrorContext(ctx, "candidate pass failed", "error", err)
			}
		}
	}
}

// Once provisions one candidate run per programme that has one due.
func (c *CandidatePass) Once(ctx context.Context) (int, error) {
	queries := sqlcgen.New(c.pool)

	due, err := queries.ProgramsDueForCandidates(ctx, sqlcgen.ProgramsDueForCandidatesParams{
		At: stamp(c.scheduler.now()),
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

// provision defines and starts one candidate run and reports whether it went out.
// Same shape as the due-date pass: the run is committed before starting, or an
// execution could run with a valid credential against a rolled-back row.
func (c *CandidatePass) provision(ctx context.Context, org, program uuid.UUID, name string) bool {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		c.log.ErrorContext(ctx, "begin failed", "program", program, "error", err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	definition, err := c.scheduler.Candidate(ctx, sqlcgen.New(tx), org, program)
	switch {
	case errors.Is(err, ErrNothingDue):
		// The last due candidate was taken between the statements: on this lane the
		// normal outcome of a busy apex, not a condition to report.
		return false
	case errors.Is(err, ErrRunInFlight):
		return false
	case err != nil:
		c.log.WarnContext(ctx, "candidate run refused",
			"program", program, "name", name, "error", err)
		return false
	}

	if err := tx.Commit(ctx); err != nil {
		c.log.ErrorContext(ctx, "commit failed", "program", program, "error", err)
		return false
	}

	record := func(ctx context.Context, runID uuid.UUID, external string) error {
		return sqlcgen.New(c.pool).RecordRunStart(ctx, sqlcgen.RecordRunStartParams{
			RunID: pgUUID(runID), ExternalID: external,
		})
	}
	if err := c.scheduler.Launch(ctx, record, definition); err != nil {
		c.log.ErrorContext(ctx, "candidate run not started",
			"program", program, "name", name, "run", definition.RunID, "error", err)
		return false
	}
	return true
}
