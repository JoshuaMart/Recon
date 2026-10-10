package runs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// DuePass is the pass on due dates, counterpart of the discovery cadence: enumeration asks
// what exists under a perimeter, this asks what still answers, turning the due dates every
// ingestion writes into runs. It was the missing half: selection, frozen list and lease
// existed but nothing called them on a cadence, so runs went out only on a button press while
// due dates piled up in the queue view.
type DuePass struct {
	pool      *pgxpool.Pool
	scheduler *Scheduler
	interval  time.Duration
	log       *slog.Logger
}

// NewDuePass builds the loop.
func NewDuePass(pool *pgxpool.Pool, scheduler *Scheduler, interval time.Duration, log *slog.Logger) *DuePass {
	if interval <= 0 {
		interval = time.Minute
	}
	return &DuePass{pool: pool, scheduler: scheduler, interval: interval, log: log}
}

// Run ticks until the context ends. The interval is how fast the pass reacts, not
// how much it starts: due dates and the one live verification run per programme
// bound the runs, so an empty tick costs a query. A short one sends a declared
// asset out at once, as manual entry writes a due date of now.
func (d *DuePass) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.Once(ctx); err != nil {
				d.log.ErrorContext(ctx, "due date pass failed", "error", err)
			}
		}
	}
}

// Once provisions one verification run per programme that has work.
func (d *DuePass) Once(ctx context.Context) (int, error) {
	queries := sqlcgen.New(d.pool)

	due, err := queries.ProgramsDueForVerification(ctx, sqlcgen.ProgramsDueForVerificationParams{
		At: stamp(d.scheduler.now()),
	})
	if err != nil {
		return 0, err
	}

	started := 0
	for _, program := range due {
		if ctx.Err() != nil {
			return started, ctx.Err()
		}
		// Full first where anything is due for it: a full run executes every rung below,
		// and its report moves both dates, so this cannot starve resolve.
		rung := lifecycle.RungResolve
		if program.FullDue {
			rung = lifecycle.RungFull
		}
		if d.provision(ctx, uuid.UUID(program.OrgID.Bytes), uuid.UUID(program.ID.Bytes),
			program.Name, rung) {
			started++
		}
	}
	return started, nil
}

// provision defines and starts one run, and reports whether it went out.
func (d *DuePass) provision(ctx context.Context, org, program uuid.UUID, name, rung string) bool {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "begin failed", "program", program, "error", err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	definition, err := d.scheduler.Verification(ctx, sqlcgen.New(tx), org, program, rung)
	switch {
	case errors.Is(err, ErrNothingDue):
		// Not a failure: the last due asset was taken between the two statements.
		return false
	case errors.Is(err, ErrRunInFlight):
		// The selection already excludes these; this is the race the unique index is for.
		return false
	case err != nil:
		d.log.WarnContext(ctx, "verification refused",
			"program", program, "name", name, "rung", rung, "error", err)
		return false
	}

	if err := tx.Commit(ctx); err != nil {
		d.log.ErrorContext(ctx, "commit failed", "program", program, "error", err)
		return false
	}

	// After the commit, as in the discovery cadence: starting inside the transaction
	// could leave an execution against a rolled-back row. A refusing platform leaves a
	// row the sweeper owns, due dates unmoved, and the next tick starts a fresh run.
	record := func(ctx context.Context, runID uuid.UUID, external string) error {
		return sqlcgen.New(d.pool).RecordRunStart(ctx, sqlcgen.RecordRunStartParams{
			RunID: pgUUID(runID), ExternalID: external,
		})
	}
	if err := d.scheduler.Launch(ctx, record, definition); err != nil {
		d.log.ErrorContext(ctx, "verification run not started",
			"program", program, "name", name, "run", definition.RunID, "error", err)
		return false
	}
	return true
}
