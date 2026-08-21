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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxEntryBytes bounds a hand entered list. It is a person typing, not a
// scanner delivering, so the bound is small and the refusal is readable.
const maxEntryBytes = 1 << 20

// maxEntries bounds one call. A list longer than this is an import, and an
// import belongs behind a job rather than behind a request somebody is waiting
// on.
const maxEntries = 5000

// Programs holds the routes a console drives.
type Programs struct {
	pool      *pgxpool.Pool
	scheduler *runs.Scheduler
	ingestor  *ingest.Ingestor
	now       func() time.Time
	log       *slog.Logger
}

// NewPrograms builds them.
func NewPrograms(pool *pgxpool.Pool, scheduler *runs.Scheduler, ingestor *ingest.Ingestor, log *slog.Logger) *Programs {
	return &Programs{pool: pool, scheduler: scheduler, ingestor: ingestor, now: time.Now, log: log}
}

// StartRun provisions one run over a programme.
//
// It exists beside the cadence rather than instead of it: the scheduled pass
// gives regular coverage, and this is what re-runs a perimeter after a scope
// change without waiting for the next tick.
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

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	var definition *runs.Definition
	switch body.Kind {
	case runs.KindDiscovery:
		definition, err = h.scheduler.Discovery(ctx, q, principal.OrgID, programID)
	case runs.KindVerification:
		definition, err = h.scheduler.Verification(ctx, q, principal.OrgID, programID, body.Scope)
	default:
		fail(w, http.StatusBadRequest, "unknown_kind", fmt.Sprintf("run kind %q is neither discovery nor verification", body.Kind))
		return
	}

	switch {
	case errors.Is(err, runs.ErrNothingDue):
		// Not a failure. A tick that finds nothing to do is the normal state
		// of a healthy inventory, and answering 200 with a count is what lets
		// a console say so instead of showing an error.
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "reason": "nothing is due"})
		return
	case errors.Is(err, runs.ErrNoPerimeter):
		fail(w, http.StatusConflict, "no_perimeter",
			"the programme declares no apex, so there is nothing to enumerate")
		return
	case errors.Is(err, runs.ErrRunInFlight):
		// The message names the run, its state and its age, because two
		// situations here call for opposite actions: a run something has
		// opened is a run to wait for, and a run nobody has claimed is a run
		// whose provisioning failed.
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

	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":      true,
		"run_id":       definition.RunID,
		"kind":         definition.Kind,
		"scope":        definition.Scope,
		"deadline":     definition.Deadline,
		"target_count": definition.TargetCount,
		// The environment map the job definition is started with. In
		// development nothing starts it, so the console shows it and a person
		// runs the image: the same shape as production minus the call.
		"env": definition.Env,
	})
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

	tx, err := h.pool.Begin(ctx)
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

// owns refuses a programme that is not this caller's.
//
// A 404 rather than a 403: a caller learning that an identifier exists in
// another organization is a cross-tenant leak of exactly one bit, and one bit
// is enough to enumerate.
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

// compileScope reads the rules in force at an instant.
//
// The instant is a parameter rather than now(): valid_to is written by the
// application and now() is the database's clock, so comparing the two would
// make the answer depend on two clocks agreeing.
func compileScope(ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, at time.Time) (*scope.Set, error) {
	rows, err := q.ListScopeRules(ctx, sqlcgen.ListScopeRulesParams{
		ProgramID: uuidTo(programID),
		At:        stamp(at),
	})
	if err != nil {
		return nil, fmt.Errorf("list scope rules: %w", err)
	}

	rules := make([]scope.Rule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, scope.Rule{
			ID:      uuid.UUID(row.ID.Bytes).String(),
			Kind:    row.Kind,
			Matcher: row.Matcher,
			Pattern: row.Pattern,
		})
	}
	return scope.Compile(rules)
}
