// Package api is the control plane's HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxReportBytes bounds a report body.
//
// A large perimeter produces a large document, so this is generous. It is not
// absent, because the one thing a run must not be able to do is exhaust the
// process that reads it.
const maxReportBytes = 64 << 20

// Reports ingests what a run produced.
type Reports struct {
	pool     *pgxpool.Pool
	signer   *auth.Signer
	ingestor *ingest.Ingestor
	now      func() time.Time
	log      *slog.Logger
}

// NewReports builds the handler.
func NewReports(pool *pgxpool.Pool, signer *auth.Signer, ingestor *ingest.Ingestor, log *slog.Logger) *Reports {
	return &Reports{pool: pool, signer: signer, ingestor: ingestor, now: time.Now, log: log}
}

// ServeHTTP accepts one report.
//
// The order of the checks is the contract. The run is read from the token's
// claims and never from the body: reading it from the body first would let
// anyone probe an organization's run states by the shape of the answer.
func (h *Reports) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	runID, err := h.authenticate(r)
	if err != nil {
		// One answer to the outside for a missing, wrong or expired
		// credential. Which one it was belongs in a log, not in a reply.
		h.log.WarnContext(ctx, "report refused", "reason", err)
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}

	var report ingest.Report
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReportBytes))
	if err := decoder.Decode(&report); err != nil {
		fail(w, http.StatusBadRequest, "malformed", "the body is not a report document")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	run, err := q.RunForIngest(ctx, sqlcgen.RunForIngestParams{RunID: uuidTo(runID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
			return
		}
		h.log.ErrorContext(ctx, "read run failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	// A run in a terminal state rejects any later report bearing its id. That
	// is the effective revocation: a signed token cannot be recalled, so it
	// stays valid and stops being useful.
	if run.State == "completed" || run.State == "failed" || run.State == "expired" {
		fail(w, http.StatusConflict, "run_closed",
			fmt.Sprintf("this run is %s and accepts nothing further", run.State))
		return
	}

	// A run that started before an expiry must not write after it.
	if reason, ok := h.programRefuses(run); !ok {
		fail(w, http.StatusForbidden, "program_unauthorized", reason)
		return
	}

	set, targets, err := h.perimeter(ctx, q, run)
	if err != nil {
		h.log.ErrorContext(ctx, "read perimeter failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	summary, err := h.ingestor.Report(ctx, q, ingest.Run{
		ID:        runID,
		OrgID:     uuid.UUID(run.OrgID.Bytes),
		ProgramID: uuid.UUID(run.ProgramID.Bytes),
		Kind:      run.Kind,
		Targets:   targets,
		Due:       ingest.DefaultSchedule(h.now(), false),
	}, set, report)
	if err != nil {
		h.log.ErrorContext(ctx, "ingest failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		encoded = []byte("{}")
	}
	if err := q.CloseRun(ctx, sqlcgen.CloseRunParams{
		RunID:   uuidTo(runID),
		State:   closingState(report),
		At:      stamp(h.now()),
		Summary: encoded,
	}); err != nil {
		h.log.ErrorContext(ctx, "close run failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.log.ErrorContext(ctx, "commit failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	h.log.InfoContext(ctx, "report ingested",
		"run", runID, "hosts", summary.Hosts, "assets", summary.Assets,
		"observations", summary.Observations, "deduplicated", summary.Deduplicated,
		"rejected", summary.Rejected)

	writeJSON(w, http.StatusOK, summary)
}

func (h *Reports) authenticate(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return uuid.Nil, auth.ErrMissing
	}
	return h.signer.Verify(auth.PurposeReport, strings.TrimSpace(token), h.now())
}

func (h *Reports) programRefuses(run sqlcgen.RunForIngestRow) (string, bool) {
	if run.ProgramState != "active" {
		return "the programme is " + run.ProgramState, false
	}
	now := h.now()
	if run.AuthorizedFrom.Valid && now.Before(run.AuthorizedFrom.Time) {
		return "the authorization has not started", false
	}
	if run.AuthorizedTo.Valid && !now.Before(run.AuthorizedTo.Time) {
		return "the authorization has expired", false
	}
	return "", true
}

// perimeter reads the rules in force and, on a verification run, the frozen
// target list. A discovery run has none, because it is the one allowed to find
// things.
func (h *Reports) perimeter(
	ctx context.Context, q *sqlcgen.Queries, run sqlcgen.RunForIngestRow,
) (*scope.Set, map[string]struct{}, error) {
	rows, err := q.ListScopeRules(ctx, sqlcgen.ListScopeRulesParams{
		ProgramID: run.ProgramID,
		// The instant is a parameter rather than now(): a date comparison
		// never mixes the application's clock with the database's.
		At: stamp(h.now()),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list scope rules: %w", err)
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
	set, err := scope.Compile(rules)
	if err != nil {
		return nil, nil, fmt.Errorf("compile scope: %w", err)
	}

	if run.Kind != "verification" {
		return set, nil, nil
	}

	keys, err := q.ListRunTargets(ctx, sqlcgen.ListRunTargetsParams{RunID: run.ID})
	if err != nil {
		return nil, nil, fmt.Errorf("list run targets: %w", err)
	}
	targets := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		targets[key] = struct{}{}
	}
	return set, targets, nil
}

// closingState reads what the run said about itself. A truncated run is not a
// failed one: the report was delivered and it is valid, it is simply not
// exhaustive.
func closingState(report ingest.Report) string {
	if report.Run.Completed {
		return "completed"
	}
	return "failed"
}
