package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Targets serves a run its frozen list.
//
// It is the only thing a run is given about the inventory, and it is scoped to
// the run: nothing it holds opens anything else. The credential is a signature
// over the run, the purpose and an expiry, so a token minted to fetch a list
// cannot be replayed to post a report.
type Targets struct {
	pool   *pgxpool.Pool
	signer *auth.Signer
	now    func() time.Time
	log    *slog.Logger
}

// NewTargets builds the handler.
func NewTargets(pool *pgxpool.Pool, signer *auth.Signer, log *slog.Logger) *Targets {
	return &Targets{pool: pool, signer: signer, now: time.Now, log: log}
}

// ServeHTTP answers with one canonical host per line.
//
// Plain text rather than JSON, because the consumer is a scanner reading a
// target file and the format has to be the one it already accepts. A blank
// body is a valid answer only for a run that froze nothing, which cannot
// happen: a run with no target is never created.
func (h *Targets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The run comes from the signature, and the path is checked against it.
	// Reading the run from the path first would let anyone probe an
	// organization's run states by the shape of the answer.
	signed, err := h.signer.Verify(auth.PurposeTargets, r.URL.Query().Get("token"), h.now())
	if err != nil {
		h.log.WarnContext(ctx, "target list refused", "reason", err)
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}
	asked, err := uuid.Parse(r.PathValue("run"))
	if err != nil || asked != signed {
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	run, err := q.RunForTargets(ctx, sqlcgen.RunForTargetsParams{RunID: uuidTo(signed)})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "read run failed", "run", signed, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}

	// A run in a terminal state hands out nothing further. That is the
	// effective revocation: a signed token cannot be recalled, so it stays
	// valid and stops being useful.
	switch run.State {
	case "completed", "failed", "expired":
		fail(w, http.StatusConflict, "run_closed",
			"this run is "+run.State+" and hands out nothing further")
		return
	}

	keys, err := q.ListRunTargets(ctx, sqlcgen.ListRunTargetsParams{RunID: run.ID})
	if err != nil {
		h.log.ErrorContext(ctx, "read targets failed", "run", signed, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}

	// Reaching for the list is what says a scanner opened this run, as opposed
	// to a provisioner having promised to. Those two call for opposite actions
	// when somebody finds a run sitting there, and this is the only moment
	// that separates them for a verification run.
	if err := q.MarkRunRunning(ctx, sqlcgen.MarkRunRunningParams{
		RunID: run.ID,
		At:    stamp(h.now()),
	}); err != nil {
		h.log.ErrorContext(ctx, "mark run running failed", "run", signed, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.log.ErrorContext(ctx, "commit failed", "run", signed, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join(keys, "\n") + "\n"))

	h.log.InfoContext(ctx, "target list served", "run", signed, "targets", len(keys))
}
