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
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxReportBytes bounds a report body.
//
// A large perimeter produces a large document, so this is generous. It is not
// absent, because the one thing a run must not be able to do is exhaust the
// process that reads it.
const maxReportBytes = 64 << 20

// supportedSchemaMajor is the report contract this understands. A minor bump
// adds fields, which the unknown-field counter already handles; a major one
// removes or repurposes them, which nothing here could notice.
const supportedSchemaMajor = "1"

// Reports ingests what a run produced.
type Reports struct {
	db *store.Scoped
	// system resolves the run's organization and nothing else. A run token
	// names a run rather than a tenant, so this is the lookup that discovers
	// the one the write is scoped to: once per report, never once per
	// observation, so the round trip budget of the write path does not move.
	system   *pgxpool.Pool
	signer   *auth.Signer
	ingestor *ingest.Ingestor
	now      func() time.Time
	log      *slog.Logger
}

// NewReports builds the handler.
func NewReports(db *store.Scoped, system *pgxpool.Pool, signer *auth.Signer, ingestor *ingest.Ingestor, log *slog.Logger) *Reports {
	return &Reports{db: db, system: system, signer: signer, ingestor: ingestor, now: time.Now, log: log}
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

	// The report type is transcribed rather than shared so the scanner can
	// evolve on its own cycle. That only holds if a major version this does
	// not know is refused: a document reusing field names under new meanings
	// would be ingested under the old ones and write wrong inventory in
	// silence.
	if major, _, _ := strings.Cut(report.SchemaVersion, "."); major != supportedSchemaMajor {
		fail(w, http.StatusBadRequest, "schema_unsupported",
			fmt.Sprintf("report schema %q is not %s.x", report.SchemaVersion, supportedSchemaMajor))
		return
	}

	org, err := sqlcgen.New(h.system).OrgForRun(ctx, sqlcgen.OrgForRunParams{RunID: uuidTo(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "read run organization failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	tx, err := h.db.Begin(ctx, uuid.UUID(org.Bytes))
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

	// A late report is accepted and marked as such. The data is still valid:
	// the run may simply have been re-dispatched, and deduplication merges the
	// two. Refusing it would throw away work that was actually done.
	late := run.Deadline.Valid && h.now().After(run.Deadline.Time)

	summary, err := h.ingestor.Report(ctx, q, ingest.Run{
		ID:        runID,
		OrgID:     uuid.UUID(run.OrgID.Bytes),
		ProgramID: uuid.UUID(run.ProgramID.Bytes),
		Kind:      run.Kind,
		Scope:     run.Scope,
		Targets:   targets,
		Grace: notify.Grace{
			CompletedDiscovery: run.CompletedDiscovery,
			AnyDiscovery:       run.AnyDiscovery,
			Assets:             int(run.Assets),
			CreatedAt:          run.ProgramCreatedAt.Time,
		},
		Due: ingest.DefaultSchedule(h.now(), false),
	}, set, report)
	if err != nil {
		h.log.ErrorContext(ctx, "ingest failed", "run", runID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the report could not be written")
		return
	}

	summary.Late = late
	encoded, err := json.Marshal(struct {
		ingest.Summary
		// What the run said about itself, kept beside what it wrote. Running
		// out of time is data rather than an error, and a run that delivered
		// nine hundred hosts before its deadline must stay distinguishable
		// from one that crashed on the first.
		Completed          bool     `json:"completed"`
		TruncatedByTimeout bool     `json:"truncated_by_timeout"`
		Degraded           []string `json:"degraded,omitempty"`
	}{
		Summary:            summary,
		Completed:          report.Run.Completed,
		TruncatedByTimeout: report.Run.TruncatedByTimeout,
		Degraded:           report.Run.Degraded,
	})
	if err != nil {
		encoded = []byte("{}")
	}
	if err := q.CloseRun(ctx, sqlcgen.CloseRunParams{
		RunID:   uuidTo(runID),
		State:   "completed",
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
		"run", runID, "sources", summary.Queried(),
		"hosts", summary.Hosts, "assets", summary.Assets,
		"created", summary.Created, "observations", summary.Observations,
		"deduplicated", summary.Deduplicated, "rejected", summary.Rejected,
		"complete", report.Run.Completed, "late", late, "degraded", report.Run.Degraded)

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

// A run that delivered a report is completed, whatever its scope reached.
//
// This used to close a truncated run as failed, contradicting the sentence that
// stood above it. Running out of time is data rather than an error: the report
// was delivered and it is valid, it is simply not exhaustive, and a scheduler
// reading "failed" would re-run work whose results it already holds. `failed`
// and `expired` belong to a run that delivered nothing, and the deadline
// sweeper owns those. What the run said about its own completeness is recorded
// in the summary, where it stays legible.
