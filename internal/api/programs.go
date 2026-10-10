package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxEntryBytes bounds a hand-entered list: a person typing, so the bound is
// small and the refusal readable.
const maxEntryBytes = 1 << 20

// maxEntries bounds one call. A longer list is an import, which belongs behind a
// job and not a request someone is waiting on.
const maxEntries = 5000

// Programs holds the routes a console drives.
type Programs struct {
	db        *store.Scoped
	scheduler *runs.Scheduler
	ingestor  *ingest.Ingestor
	// launchTimeout bounds the work that outlives the request, which would otherwise
	// have no bound.
	launchTimeout time.Duration
	now           func() time.Time
	log           *slog.Logger
}

// NewPrograms builds them.
func NewPrograms(db *store.Scoped, scheduler *runs.Scheduler, ingestor *ingest.Ingestor, launchTimeout time.Duration, log *slog.Logger) *Programs {
	if launchTimeout <= 0 {
		launchTimeout = time.Minute
	}
	return &Programs{
		db: db, scheduler: scheduler, ingestor: ingestor,
		launchTimeout: launchTimeout, now: time.Now, log: log,
	}
}

// StartRun provisions one run over a programme. It sits beside the cadence: the
// scheduled pass gives regular coverage, and this re-runs a perimeter after a
// scope change without waiting for the next tick.
func (h *Programs) StartRun(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}

	var body struct {
		Kind  string `json:"kind"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "malformed", "the body is not a run request")
		return
	}
	if body.Kind == "" {
		body.Kind = runs.KindVerification
	}
	if body.Scope == "" {
		body.Scope = "full"
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	// A list, and a verification is a list of one. The scanner takes one root domain
	// per execution, so three apexes are three runs, and a response carrying one
	// would have to pick which to report.
	var definitions []*runs.Definition
	switch body.Kind {
	case runs.KindDiscovery:
		definitions, err = h.scheduler.Discovery(ctx, q, principal.OrgID, programID)
	case runs.KindVerification:
		var one *runs.Definition
		one, err = h.scheduler.Verification(ctx, q, principal.OrgID, programID, body.Scope)
		if one != nil {
			definitions = []*runs.Definition{one}
		}
	default:
		fail(w, http.StatusBadRequest, "unknown_kind", fmt.Sprintf("run kind %q is neither discovery nor verification", body.Kind))
		return
	}

	switch {
	case errors.Is(err, runs.ErrNothingDue):
		// Not a failure: nothing due is the normal state of a healthy inventory, and a
		// 200 with a count lets the console say so.
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "reason": "nothing is due"})
		return
	case errors.Is(err, runs.ErrNoPerimeter):
		fail(w, http.StatusConflict, "no_perimeter",
			"the programme declares no apex, so there is nothing to enumerate")
		return
	case errors.Is(err, runs.ErrRunInFlight):
		// The message names the run, state and age because the two situations call for
		// opposite actions: a run something opened is one to wait for, one nobody
		// claimed is a failed provisioning.
		fail(w, http.StatusConflict, "run_in_flight", err.Error())
		return
	case err != nil:
		h.log.WarnContext(ctx, "run refused", "program", programID, "error", err)
		fail(w, http.StatusConflict, "refused", err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}

	// After the commit: starting inside the transaction could leave an execution running with a
	// valid credential against a rolled-back row. And detached from the request: a console
	// hanging up mid-call would cancel a start the platform already accepted, or the write
	// recording its name, leaving an execution with no findable logs.
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.launchTimeout)
	defer cancel()
	// The write naming the execution opens its own scoped transaction after the
	// platform answers, not around the call: holding one open while another control
	// plane replies holds a pool connection just as long.
	record := func(ctx context.Context, runID uuid.UUID, external string) error {
		tx, err := h.db.Begin(ctx, principal.OrgID)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := sqlcgen.New(tx).RecordRunStart(ctx, sqlcgen.RecordRunStartParams{
			RunID: uuidTo(runID), ExternalID: external,
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// One entry per run, started or not. A platform refusing the third of three
	// leaves a row the sweeper owns and two enumerations walking, which one boolean
	// would describe neither of.
	started := 0
	reported := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		entry := map[string]any{
			"run_id":       definition.RunID,
			"kind":         definition.Kind,
			"scope":        definition.Scope,
			"deadline":     definition.Deadline,
			"target_count": definition.TargetCount,
			// What the run was asked to do. In development nothing starts it, so the console
			// shows this and a person runs the image.
			"args": definition.Args,
			"env":  definition.Env,
		}
		// Only a discovery has one; "" would read as a domain nobody named.
		if definition.Apex != "" {
			entry["apex"] = definition.Apex
		}

		if err := h.scheduler.Launch(launchCtx, record, definition); err != nil {
			// The run exists and the sweeper owns it. Reported, not turned into a failure the
			// caller might retry into a second run.
			h.log.ErrorContext(ctx, "run not started",
				"run", definition.RunID, "apex", definition.Apex, "error", err)
			entry["started"] = false
			entry["reason"] = "the platform did not start it, and the deadline sweeper owns the run"
			reported = append(reported, entry)
			continue
		}
		entry["started"] = true
		entry["external_id"] = definition.ExternalID
		reported = append(reported, entry)
		started++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		// True when anything went out; the list says what did and did not.
		"started": started > 0,
		"runs":    reported,
	})
}

// CancelRun closes an open run and asks the platform to stop its execution, for
// what the sweeper does not cover: a run started by mistake, or an execution the
// platform calls running while doing nothing useful. The run closes failed, so a
// discovery falls under the ordinary retry delay.
func (h *Programs) CancelRun(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	runID, ok := pathUUID(w, r, "run")
	if !ok {
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	external, err := h.scheduler.Cancel(ctx, sqlcgen.New(tx), runID)
	switch {
	case errors.Is(err, runs.ErrRunNotFound):
		fail(w, http.StatusNotFound, "not_found", "no such run")
		return
	case errors.Is(err, runs.ErrReportInProgress):
		fail(w, http.StatusConflict, "report_in_progress",
			"the run's report is being written, and the run will be completed in a moment")
		return
	case errors.Is(err, runs.ErrRunClosed):
		fail(w, http.StatusConflict, "run_closed", err.Error())
		return
	case err != nil:
		h.unavailable(ctx, w, "cancel failed", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}

	// After the commit and detached, as in StartRun. A stop the platform refuses does
	// not reopen the run: its token is refused from now on.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.launchTimeout)
	defer cancel()
	stopped, err := h.scheduler.Stop(stopCtx, external)
	if err != nil {
		h.log.ErrorContext(ctx, "run cancelled, the platform did not stop it",
			"run", runID, "external", external, "error", err)
	}
	h.log.InfoContext(ctx, "run cancelled", "run", runID, "external", external, "stopped", stopped)

	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "state": "failed", "stopped": stopped})
}

// EnterAssets records assets somebody typed in.
func (h *Programs) EnterAssets(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}

	var body struct {
		Entries []string `json:"entries"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEntryBytes)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "malformed", "the body is not a list of entries")
		return
	}
	if len(body.Entries) == 0 {
		fail(w, http.StatusBadRequest, "empty", "no entry was given")
		return
	}
	if len(body.Entries) > maxEntries {
		fail(w, http.StatusRequestEntityTooLarge, "too_many",
			fmt.Sprintf("%d entries exceeds the bound of %d", len(body.Entries), maxEntries))
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	set, err := compileScope(ctx, q, programID, h.now())
	if err != nil {
		h.unavailable(ctx, w, "read perimeter failed", err)
		return
	}

	entered, err := h.ingestor.Enter(ctx, q, ingest.Run{
		ID:        uuid.New(),
		OrgID:     principal.OrgID,
		ProgramID: programID,
		Kind:      "manual",
	}, set, body.Entries)
	if err != nil {
		h.unavailable(ctx, w, "manual entry failed", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}

	h.log.InfoContext(ctx, "assets entered by hand",
		"program", programID, "accepted", len(entered.Accepted), "refused", len(entered.Refused))
	writeJSON(w, http.StatusOK, entered)
}

// owns refuses a programme that is not this caller's, as a 404 and not a 403:
// learning that an identifier exists in another organization is a one-bit
// cross-tenant leak, and one bit is enough to enumerate.
func (h *Programs) owns(
	ctx context.Context, w http.ResponseWriter, q *sqlcgen.Queries,
	principal auth.Principal, programID uuid.UUID,
) bool {
	row, err := q.ProgramForScheduling(ctx, sqlcgen.ProgramForSchedulingParams{
		ProgramID: uuidTo(programID),
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && uuid.UUID(row.OrgID.Bytes) != principal.OrgID) {
		fail(w, http.StatusNotFound, "not_found", "no such programme")
		return false
	}
	if err != nil {
		h.unavailable(ctx, w, "read programme failed", err)
		return false
	}
	return true
}

func (h *Programs) unavailable(ctx context.Context, w http.ResponseWriter, message string, err error) {
	h.log.ErrorContext(ctx, message, "error", err)
	fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
}

// compileScope reads the rules in force at an instant. It lives in ingest
// because the CT matcher needs it too, and a second copy would be a second answer
// to what a programme may look at.
func compileScope(ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, at time.Time) (*scope.Set, error) {
	return ingest.CompileScope(ctx, q, programID, at)
}
