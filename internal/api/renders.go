package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Renders are the two API entry-point triggers. Both require manage_jobs, not
// ingest: ingest could otherwise schedule renders of its choosing and spend a
// programme's budget on targets it picked.
type Renders struct {
	db     *store.Scoped
	spread time.Duration
	now    func() time.Time
	log    *slog.Logger
}

// NewRenders builds them.
func NewRenders(db *store.Scoped, spread time.Duration, log *slog.Logger) *Renders {
	return &Renders{db: db, spread: spread, now: time.Now, log: log}
}

// Request puts one asset at the head of the queue.
func (h *Renders) Request(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	assetID, ok := pathUUID(w, r, "asset")
	if !ok {
		return
	}

	// A transaction, the only shape that can carry an organization (the policies read
	// a transaction-scoped variable). The two reads and the write then also see one
	// consistent state.
	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := sqlcgen.New(tx)
	row, err := queries.AssetForRender(ctx, sqlcgen.AssetForRenderParams{AssetID: uuidTo(assetID)})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && uuid.UUID(row.OrgID.Bytes) != principal.OrgID) {
		// An identifier existing in another organization is a one-bit cross-tenant leak,
		// and one bit is enough to enumerate.
		fail(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "read asset failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	// The baseline's filter: a browser will not open some ports, so promoting one puts
	// an asset at the head of a queue that can never serve it, and a 200 would tell the
	// caller to wait for a render nobody will make.
	if row.Port != nil && !fingerprint.Renderable(int(*row.Port)) {
		writeJSON(w, http.StatusOK, map[string]any{
			"asset_id": assetID,
			"key":      row.Key,
			"queued":   false,
			"reason":   "a browser refuses this port, so no render is made for it",
		})
		return
	}

	at := h.now()
	if err := queries.PromoteRender(ctx, sqlcgen.PromoteRenderParams{
		AssetID:  uuidTo(assetID),
		At:       stamp(at),
		Priority: lifecycle.PriorityChange,
	}); err != nil {
		h.log.ErrorContext(ctx, "promote render failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	// The statement refuses an asset that left the scheduler, so the answer says
	// whether anything is queued instead of claiming a render never selected.
	after, err := queries.AssetForRender(ctx, sqlcgen.AssetForRenderParams{AssetID: uuidTo(assetID)})
	if err != nil {
		h.log.ErrorContext(ctx, "read asset failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	// Read from the statement's own conditions, not one of them: it refuses assets
	// that left the scheduler and ones outside the perimeter alike, and answering on
	// the lifecycle alone told callers to wait for a render nothing would select.
	if err := tx.Commit(ctx); err != nil {
		h.log.ErrorContext(ctx, "commit failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	queued := after.Lifecycle != lifecycle.Archived && after.ScopeStatus == "in_scope"
	writeJSON(w, http.StatusOK, map[string]any{
		"asset_id": assetID,
		"key":      after.Key,
		"queued":   queued,
	})
}

// Replan is the forced refresh after a major update of the rendering service. The
// whole inventory returns to the low queue, spread over days: baselines are
// restored without a mass alert, which an hour would be.
func (h *Renders) Replan(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	moved, err := sqlcgen.New(tx).ReplanRenders(ctx, sqlcgen.ReplanRendersParams{
		OrgID:         uuidTo(principal.OrgID),
		At:            stamp(h.now()),
		SpreadSeconds: int64(h.spread / time.Second),
	})
	if err != nil {
		h.log.ErrorContext(ctx, "replan failed", "org", principal.OrgID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.log.ErrorContext(ctx, "commit failed", "org", principal.OrgID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	h.log.InfoContext(ctx, "renders replanned",
		"org", principal.OrgID, "assets", moved, "spread", h.spread.String())
	writeJSON(w, http.StatusOK, map[string]any{
		"replanned": moved,
		"spread":    h.spread.String(),
	})
}
