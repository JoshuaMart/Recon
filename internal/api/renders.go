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

// Renders are the two triggers that are API entry points.
//
// Both require manage_jobs rather than ingest, and the distinction is not
// tidiness: something holding ingest could otherwise schedule renders of its
// choosing and spend a programme's budget on targets it picked.
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

	// A transaction rather than a bare query, because that is the only shape
	// that can carry an organization: the policies read a variable that is
	// transaction scoped. The two reads and the write below then see one
	// consistent state as a side effect, which they did not before.
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
		// A caller learning that an identifier exists in another organization
		// is a cross-tenant leak of exactly one bit, and one bit is enough to
		// enumerate.
		fail(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "read asset failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	// The same filter the baseline reads. A browser will not open some ports at
	// all, so promoting one puts an asset at the head of a queue that can never
	// serve it, and answering 200 tells the caller to wait for a render nobody
	// will make.
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

	// The statement refuses an asset that has left the scheduler, so the
	// answer says whether anything is actually queued rather than claiming a
	// render that will never be selected.
	after, err := queries.AssetForRender(ctx, sqlcgen.AssetForRenderParams{AssetID: uuidTo(assetID)})
	if err != nil {
		h.log.ErrorContext(ctx, "read asset failed", "asset", assetID, "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
		return
	}

	// Read from the statement's own conditions rather than from one of them.
	// It refuses an asset that has left the scheduler and one outside the
	// perimeter alike, and answering on the lifecycle alone told a caller to
	// wait for a render nothing would ever select.
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

// Replan is the forced refresh after a major update of the rendering service.
//
// The whole inventory goes back into the low queue, spread over several days.
// That restores baseline consistency without a mass alert, and doing it in an
// hour would be the mass alert.
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
