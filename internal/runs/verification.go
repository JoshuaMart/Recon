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

// DuePass is the pass on due dates, and it is the counterpart of the discovery
// cadence rather than a variant of it.
//
// The two answer different questions. Enumeration asks what exists under a
// perimeter and runs on the programme's own interval. This asks what still
// answers, and it runs on the assets: a due date is written by every ingestion,
// and this is what turns it into a run.
//
// It was the missing half. The selection, the frozen list and the lease have
// existed since verification was built, and nothing called them on a cadence,
// so a run went out when somebody pressed a button and never otherwise. Due
// dates accumulated, the queue view counted them as what the next tick can
// dispatch, and no tick did.
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

// Run ticks until the context ends.
//
// The interval is how fast the pass reacts and not how much it starts. What
// bounds the runs is the due dates and the one live verification run per
// programme, so a tick that finds nothing due starts nothing and a short
// interval costs a query rather than an execution. Short is what makes a
// declared asset go out at once: manual entry writes a due date of now, because
// somebody is waiting.
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
		// Full first where anything is due for it. A full run executes every
		// rung below it and its report moves both dates, so this cannot starve
		// resolve, and an asset due for full does not need a resolve run.
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
		// Not a failure, and reaching it means the last due asset was taken
		// between the two statements. A tick that finds nothing to do is the
		// normal state of a healthy inventory.
		return false
	case errors.Is(err, ErrRunInFlight):
		// The selection already excludes these, so this is the race the unique
		// index exists for rather than a condition to report.
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

	// After the commit, deliberately, and for the reason the discovery cadence
	// gives: starting inside the transaction would leave an execution running
	// against a run row that was rolled back. A platform that refuses on a quota
	// leaves a row the deadline sweeper owns, the due dates were never moved,
	// and the next tick starts a fresh run over the same assets.
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
