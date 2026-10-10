package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store"
)

// How the stream is bounded; neither bound concerns display.
const (
	// feedInterval is how often a round runs. Discovery arrives in batches (a run
	// posts one report), so sub-second latency describes nothing real.
	feedInterval = 3 * time.Second
	// feedLife ends the stream on its own and the client reconnects, as SSE does by
	// itself. A forgotten tab would hold an HTTP request forever, a leak visible only
	// in the connection count.
	feedLife = 5 * time.Minute
	// feedHeartbeat keeps a proxy from cutting a silent connection, which the browser
	// does not report. It is a comment line, dropped by the protocol and not mistaken
	// for data.
	feedHeartbeat = 20 * time.Second
)

// Feed streams discoveries.
type Feed struct {
	db  *store.Scoped
	now func() time.Time
	log *slog.Logger
}

// NewFeed builds it.
func NewFeed(db *store.Scoped, log *slog.Logger) *Feed {
	return &Feed{db: db, now: time.Now, log: log}
}

// Stream answers the live feed. Polling with a cursor, not a database
// notification: NOTIFY adds a round trip to the hottest write path, is global (a
// subscriber would receive every organization's channels), and LISTEN pins a pool
// connection per open tab.
func (h *Feed) Stream(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	// Last-Event-ID first: it is the browser's own resumption and arrives for free.
	// The query parameter serves a first connection that already knows its position.
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("cursor")
	}
	cursor, err := search.ParseFeedCursor(raw)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_cursor", err.Error())
		return
	}
	if cursor.Zero() {
		// At the present, not the start of the inventory: a tab opening on a feed asks what
		// is happening now, and replaying what the list shows wastes its first minutes.
		cursor = search.Head(h.now())
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Every server this runs behind supports it, but a stream that buffers delivers
		// nothing and reports no error, so it is checked.
		fail(w, http.StatusInternalServerError, "unavailable", "this server cannot stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// The one header that is not decoration: a buffering reverse proxy holds an event
	// stream until it has enough bytes, which for a mostly silent feed is forever.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(feedInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(feedLife)
	defer deadline.Stop()

	silent := h.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}

		tick, err := h.round(ctx, principal, cursor)
		if err != nil {
			// The connection is open, so this cannot become a status. It is logged and the
			// stream ends; the client reconnects with its last id, so a failed round costs a
			// reconnection, never a gap.
			h.log.ErrorContext(ctx, "feed round failed", "org", principal.OrgID, "error", err)
			return
		}

		if len(tick.Discoveries) == 0 {
			// A round finding nothing emits nothing and the cursor stays: an empty event per
			// tick would advance the id, and a reconnecting client would resume from a
			// position that never named a discovery.
			if h.now().Sub(silent) >= feedHeartbeat {
				_, _ = w.Write([]byte(": still here\n\n"))
				flusher.Flush()
				silent = h.now()
			}
			continue
		}

		payload, err := json.Marshal(tick)
		if err != nil {
			h.log.ErrorContext(ctx, "feed encode failed", "error", err)
			return
		}
		// The id is the cursor, which makes resumption free: the Last-Event-ID the browser
		// returns suffices, with no server-side state.
		//
		// The event is named so clients listen for discoveries only. An unnamed event
		// arrives as "message", and adding a second message kind later would reach every
		// existing listener.
		if _, err := w.Write([]byte(
			"id: " + tick.Cursor + "\nevent: discoveries\ndata: " + string(payload) + "\n\n")); err != nil {
			return
		}
		flusher.Flush()
		silent = h.now()
		cursor, err = search.ParseFeedCursor(tick.Cursor)
		if err != nil {
			h.log.ErrorContext(ctx, "feed cursor unreadable", "cursor", tick.Cursor, "error", err)
			return
		}
	}
}

// round runs one poll in its own short transaction. One held open for the stream's
// life would hold a pool connection for five minutes and read an ever older
// snapshot, which a live feed cannot have.
func (h *Feed) round(
	ctx context.Context, principal auth.Principal, cursor search.FeedCursor,
) (search.Tick, error) {
	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		return search.Tick{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	return search.Discoveries(ctx, tx, principal.OrgID, cursor, h.now())
}
