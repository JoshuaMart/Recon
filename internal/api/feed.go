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

// How the stream is bounded, and neither bound has anything to do with display.
const (
	// feedInterval is how often a round runs. Discovery arrives in batches: a
	// run posts one report, so sub second latency describes nothing real.
	feedInterval = 3 * time.Second
	// feedLife ends the stream on its own, after which the client reconnects,
	// which the SSE protocol does by itself. A forgotten tab holds an HTTP
	// request indefinitely, and an eternal stream is a leak visible only in the
	// connection count.
	feedLife = 5 * time.Minute
	// feedHeartbeat is what keeps a proxy from cutting a silent connection,
	// which the browser does not report. It is a comment line, which the
	// protocol drops rather than delivering as an event, so nothing about it
	// can be mistaken for data.
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

// Stream answers the live feed.
//
// Polling with a cursor rather than a database notification. A NOTIFY in the
// ingestion transaction is another round trip on the hottest write path, it is
// global so a subscriber receives every organization's channels, and LISTEN
// pins a connection, so an open tab would cost a pool connection.
func (h *Feed) Stream(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	// Last-Event-ID first, because that is the browser's own resumption and it
	// arrives without the client doing anything. The query parameter is what a
	// first connection uses when it already knows where it left off.
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
		// At the present rather than at the beginning of the inventory. A tab
		// opening on a feed is asking what is happening now, and replaying an
		// inventory the list already shows would spend the first minutes of
		// every connection on it.
		cursor = search.Head(h.now())
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Every server this runs behind supports it. Said out loud anyway,
		// because a stream that buffers is a stream that delivers nothing and
		// reports no error.
		fail(w, http.StatusInternalServerError, "unavailable", "this server cannot stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// The one header that is not decoration. A buffering reverse proxy holds an
	// event stream until it has enough bytes, which for a feed that emits
	// nothing most of the time is forever.
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
			// The connection is already open, so this cannot become a status.
			// It is logged and the stream ends, which the client answers by
			// reconnecting with the id it last saw: a failed round costs a
			// reconnection and never a gap.
			h.log.ErrorContext(ctx, "feed round failed", "org", principal.OrgID, "error", err)
			return
		}

		if len(tick.Discoveries) == 0 {
			// A round that found nothing emits nothing, and the cursor stays
			// where it was. An empty event per tick would advance the id on
			// every tick, and a client reconnecting on it would resume from a
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
		// The id is the cursor, which is what makes resumption free: the
		// Last-Event-ID the browser sends back is enough, with no server side
		// state.
		//
		// The event is named, so a client listens for discoveries rather than for
		// whatever else this stream might carry later. An unnamed one arrives as
		// "message", and adding a second kind of message afterwards would mean
		// every existing listener starts receiving it.
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

// round runs one poll in its own transaction.
//
// Its own, and short. A transaction held open for the life of the stream would
// hold a pool connection for five minutes and read a snapshot that gets older
// with every round, which is the failure a live feed cannot have.
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
