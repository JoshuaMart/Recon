//go:build integration

// What the platform says about a run, and what a person can do about one. Both
// exist because a scanner that died after five minutes used to hold its apex
// for the rest of its budget while the console said it was waiting.
package runs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// started provisions one discovery run through the cadence, against a platform
// that names it run-01, and returns its id.
func (h *harness) started(t *testing.T, platform *recorder) (uuid.UUID, *runs.Scheduler) {
	t.Helper()
	ctx := context.Background()

	exec(t, h.pool, `INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern, valid_from)
		VALUES ($1,$2,$3,'include','apex','acme.test',$4)`,
		uuid.New(), h.org, h.program, h.clock.now.Add(-time.Hour))

	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(platform))
	if started, err := runs.NewCadence(h.pool, scheduler, time.Minute, quiet()).Once(ctx); err != nil || started != 1 {
		t.Fatalf("%d runs went out: %v", started, err)
	}

	var id uuid.UUID
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM run WHERE program_id = $1 AND kind = 'discovery'`, h.program).Scan(&id); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	return id, scheduler
}

func (h *harness) runState(t *testing.T, id uuid.UUID) (state string, started bool, reason string) {
	t.Helper()

	var startedAt *time.Time
	var errText *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, started_at, error FROM run WHERE id = $1`, id).Scan(&state, &startedAt, &errText); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if errText != nil {
		reason = *errText
	}
	return state, startedAt != nil, reason
}

// The run that prompted this: the scanner finished, could not deliver, and
// exited 3. The platform knew within the minute and Recon waited out the
// deadline.
func TestAnExecutionThePlatformCallsFailedClosesItsRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	platform := &recorder{id: "run-01"}
	id, scheduler := h.started(t, platform)

	code := 3
	platform.executions = map[string]runs.Execution{
		"run-01": {Failed: true, Reason: "failed, exited_with_error", ExitCode: &code},
	}
	closed, err := scheduler.Watch(ctx, h.queries)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if closed != 1 {
		t.Fatalf("%d runs closed", closed)
	}

	state, _, reason := h.runState(t, id)
	if state != "failed" {
		t.Fatalf("the run is %s", state)
	}
	for _, want := range []string{"exited_with_error", "exit code 3", "could not be delivered"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason %q does not say %q", reason, want)
		}
	}

	// And the apex is free: that was the whole complaint.
	if _, err := h.queries.LiveDiscoveryForApex(ctx, sqlcgen.LiveDiscoveryForApexParams{
		ProgramID: pgUUID(h.program), Apex: "acme.test",
	}); err == nil {
		t.Fatal("the apex is still held by a run the platform says is over")
	}
}

// A discovery run has no target list to open, so the platform saying the
// container runs is the first sign it was taken. Without it the console said
// "waiting for a scanner" for the whole of a healthy scan.
func TestAnExecutionThePlatformCallsRunningMarksTheRunTaken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	platform := &recorder{id: "run-01"}
	id, scheduler := h.started(t, platform)

	if _, err := scheduler.Watch(ctx, h.queries); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if state, started, _ := h.runState(t, id); state != "pending" || started {
		t.Fatalf("a run the platform said nothing about is %s, started %v", state, started)
	}

	platform.executions = map[string]runs.Execution{"run-01": {Running: true}}
	if _, err := scheduler.Watch(ctx, h.queries); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if state, started, _ := h.runState(t, id); state != "running" || !started {
		t.Fatalf("a running execution left the run %s, started %v", state, started)
	}
}

// A report being written holds the run row. Neither the platform's word nor
// the deadline closes it underneath, and neither waits for it: the sweeper
// serves every tenant and cannot stop for one ingestion.
func TestARunWhoseReportIsBeingWrittenIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	platform := &recorder{id: "run-01"}
	id, scheduler := h.started(t, platform)

	// What ingestion holds while it writes.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM run WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("hold the run: %v", err)
	}

	code := 3
	platform.executions = map[string]runs.Execution{"run-01": {Failed: true, ExitCode: &code}}
	h.clock.now = h.clock.now.Add(2 * time.Hour)

	done := make(chan error, 1)
	go func() {
		watchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := scheduler.Sweep(watchCtx, h.queries); err != nil {
			done <- err
			return
		}
		closed, err := scheduler.Watch(watchCtx, h.queries)
		if err == nil && closed != 0 {
			err = errors.New("the watch closed a run whose report is being written")
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := scheduler.Cancel(ctx, h.queries, id); !errors.Is(err, runs.ErrReportInProgress) {
		t.Fatalf("a cancel under an ingestion answered %v", err)
	}

	// The ingestion completes the run; the next tick has nothing to do.
	if _, err := tx.Exec(ctx, `UPDATE run SET state = 'completed', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if closed, err := scheduler.Watch(ctx, h.queries); err != nil || closed != 0 {
		t.Fatalf("a completed run was closed again: %d, %v", closed, err)
	}
	if state, _, _ := h.runState(t, id); state != "completed" {
		t.Fatalf("the run is %s", state)
	}
}

// A person can close a run, and the execution is named back so it can be
// stopped once the close is committed.
func TestACancelledRunIsClosedAndItsExecutionNamed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	platform := &recorder{id: "run-01"}
	id, scheduler := h.started(t, platform)

	external, err := scheduler.Cancel(ctx, h.queries, id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if external != "run-01" {
		t.Fatalf("the cancel named %q", external)
	}
	if state, _, reason := h.runState(t, id); state != "failed" || !strings.Contains(reason, "cancelled") {
		t.Fatalf("a cancelled run is %s: %q", state, reason)
	}

	if stopped, err := scheduler.Stop(ctx, external); err != nil || !stopped {
		t.Fatalf("stop: %v, %v", stopped, err)
	}
	if len(platform.stopped) != 1 || platform.stopped[0] != "run-01" {
		t.Fatalf("the platform was asked to stop %v", platform.stopped)
	}

	// Twice is a refusal that says why, and a run that does not exist is not
	// one that closed.
	if _, err := scheduler.Cancel(ctx, h.queries, id); !errors.Is(err, runs.ErrRunClosed) {
		t.Fatalf("a second cancel answered %v", err)
	}
	if _, err := scheduler.Cancel(ctx, h.queries, uuid.New()); !errors.Is(err, runs.ErrRunNotFound) {
		t.Fatalf("an unknown run answered %v", err)
	}
}

// The scanner is told to wait as long as Recon takes to write a report. Its
// default of thirty seconds is what lost the run that prompted this.
func TestTheScannerWaitsForTheReportToBeWritten(t *testing.T) {
	h := newHarness(t)

	platform := &recorder{id: "run-01"}
	h.started(t, platform)

	if got := arg(platform.args, "--webhook-timeout"); got != h.sched.Config().ReportTimeout.String() {
		t.Fatalf("the scanner waits %q", got)
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
