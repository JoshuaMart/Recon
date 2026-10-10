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
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Targets serves a run its frozen list, the only thing a run is given about the
// inventory, scoped to the run. The credential is a signature over run, purpose
// and expiry, so a token minted to fetch a list cannot be replayed to post a report.
type Targets struct {
	db *store.Scoped
	// system resolves the run's organization only. A run token names a run, not a
	// tenant, so this lookup discovers the tenant the rest is scoped to.
	system *pgxpool.Pool
	signer *auth.Signer
	now    func() time.Time
	log    *slog.Logger
}

// NewTargets builds the handler.
func NewTargets(db *store.Scoped, system *pgxpool.Pool, signer *auth.Signer, log *slog.Logger) *Targets {
	return &Targets{db: db, system: system, signer: signer, now: time.Now, log: log}
}

// ServeHTTP answers with one canonical host per line. Plain text, because the
// consumer is a scanner reading a target file. A blank body is valid only for a run
// that froze nothing, which cannot happen: a run with no target is never created.
func (h *Targets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The run comes from the signature and the path is checked against it; reading the
	// path first would let anyone probe an organization's run states by the shape of
	// the answer.
	//
	// The credential is a header, not a query parameter (the scanner's
	// --targets-header exists for it): a token in a URL is in every access, proxy and
	// error log that prints it, and those outlive the run.
	token, scheme := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !scheme {
		token = ""
	}
	signed, err := h.signer.Verify(auth.PurposeTargets, strings.TrimSpace(token), h.now())
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

	// One statement, outside the transaction, on the pool crossing tenants. Everything
	// after is scoped to what it returned, and the run is read again inside that
	// transaction under the policy, so a signature naming another organization's run
	// finds nothing instead of being trusted.
	org, err := sqlcgen.New(h.system).OrgForRun(ctx, sqlcgen.OrgForRunParams{RunID: uuidTo(signed)})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "read run organization failed", "run", signed, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the target list could not be read")
		return
	}

	tx, err := h.db.Begin(ctx, uuid.UUID(org.Bytes))
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

	// A run in a terminal state hands out nothing further: the effective revocation,
	// as a signed token stays valid and stops being useful.
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

	// Reaching for the list says a scanner opened this run, not that a provisioner
	// promised to. The two call for opposite actions on a stuck run, and this is the
	// only moment separating them for a verification run.
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
