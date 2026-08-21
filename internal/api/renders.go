package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Renders are the two triggers that are API entry points.
//
// Both require manage_jobs rather than ingest, and the distinction is not
// tidiness: something holding ingest could otherwise schedule renders of its
// choosing and spend a programme's budget on targets it picked.
type Renders struct {
	pool   *pgxpool.Pool
	spread time.Duration
	now    func() time.Time
	log    *slog.Logger
}

// NewRenders builds them.
func NewRenders(pool *pgxpool.Pool, spread time.Duration, log *slog.Logger) *Renders {
	return &Renders{pool: pool, spread: spread, now: time.Now, log: log}
}

// Request puts one asset at the head of the queue.
func (h *Renders) Request(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	assetID, ok := pathUUID(w, r, "asset")
	if !ok {
		return
	}

	queries := sqlcgen.New(h.pool)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"asset_id": assetID,
		"key":      after.Key,
		"queued":   after.Lifecycle != lifecycle.Archived,
	})
}

// Replan is the forced refresh after a major update of the rendering service.
//
// The whole inventory goes back into the low queue, spread over several days.
// That restores baseline consistency without a mass alert, and doing it in an
// hour would be the mass alert.
func (h *Renders) Replan(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	moved, err := sqlcgen.New(h.pool).ReplanRenders(ctx, sqlcgen.ReplanRendersParams{
		OrgID:         uuidTo(principal.OrgID),
		At:            stamp(h.now()),
		SpreadSeconds: int64(h.spread / time.Second),
	})
	if err != nil {
		h.log.ErrorContext(ctx, "replan failed", "org", principal.OrgID, "error", err)
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
