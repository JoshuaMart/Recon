package ct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"
)

// maxFrameBytes bounds one message. The feed is a public aggregator carrying
// certificates anyone can get logged, so its output is influenced by others and
// needs a ceiling. Measured on the lite stream: median 1.7 kB, p99 2.4 kB, widest
// 7.9 kB over 4000 certificates. A megabyte leaves two orders of magnitude of
// headroom and still refuses a gigabyte.
const maxFrameBytes = 1 << 20

// Feed dials the aggregator and drives the matcher with what it sends.
//
// A reconnection is a gap nothing backfills: the aggregator keeps no history for
// Recon, so what passed while the socket was down is lost and resuming starts at
// the present. Hence the matcher records the minutes it was alive instead of
// reconstructing the others, and periodic enumeration is not redundant.
type Feed struct {
	url     string
	matcher *Matcher
	log     *slog.Logger

	// Backoff bounds how fast a broken feed is redialled. A drop is usually the
	// aggregator restarting, so the first retry is quick; the ceiling stops a dead
	// endpoint being dialled in a tight loop.
	minBackoff time.Duration
	maxBackoff time.Duration
}

// NewFeed builds the socket side.
func NewFeed(url string, matcher *Matcher, log *slog.Logger) *Feed {
	return &Feed{
		url: url, matcher: matcher, log: log,
		minBackoff: time.Second,
		maxBackoff: time.Minute,
	}
}

// Run dials until the context ends.
func (f *Feed) Run(ctx context.Context) {
	backoff := f.minBackoff

	for ctx.Err() == nil {
		started := f.matcher.now()
		err := f.session(ctx)
		if ctx.Err() != nil {
			return
		}

		// A session that lasted is a working endpoint that dropped: it restarts from the
		// short delay, or a feed reconnecting every few hours would creep to the ceiling.
		if f.matcher.now().Sub(started) > f.maxBackoff {
			backoff = f.minBackoff
		}

		f.log.WarnContext(ctx, "the certificate transparency feed dropped",
			"error", err, "retry_in", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > f.maxBackoff {
			backoff = f.maxBackoff
		}
	}
}

// session holds one connection until it fails.
func (f *Feed) session(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, f.url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", f.url, err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxFrameBytes)

	f.log.InfoContext(ctx, "certificate transparency feed connected",
		"url", f.url, "apexes", f.matcher.Apexes())

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			// One unreadable message is no reason to drop a working socket. Counted, not
			// logged: at thousands of frames a second a line each would be the outage.
			f.matcher.undecodable()
			continue
		}
		f.matcher.Handle(ctx, &frame)
	}
}

// Loop reloads the apex set and flushes the counters on a tick. One ticker for
// both: counters accumulate in memory and are written when the set refreshes,
// which avoids a write per certificate.
type Loop struct {
	matcher  *Matcher
	interval time.Duration
}

// NewLoop builds the tick.
func NewLoop(matcher *Matcher) *Loop {
	interval := matcher.opts.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	return &Loop{matcher: matcher, interval: interval}
}

// Run ticks until the context ends. The first reload precedes the first tick: an
// empty set matches nothing, so waiting an interval would let a minute of
// certificates pass against an unread perimeter.
func (l *Loop) Run(ctx context.Context) {
	l.once(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The in-memory window is written on the way out, so a clean shutdown loses
			// nothing. A crash still does, which the flush being a metric allows.
			l.flush(context.WithoutCancel(ctx))
			return
		case <-ticker.C:
			l.once(ctx)
		}
	}
}

func (l *Loop) once(ctx context.Context) {
	l.flush(ctx)
	if err := l.matcher.Reload(ctx); err != nil {
		l.matcher.log.ErrorContext(ctx, "reloading the apex set failed", "error", err)
	}
}

func (l *Loop) flush(ctx context.Context) {
	if err := l.matcher.Flush(ctx); err != nil && !errors.Is(err, context.Canceled) {
		l.matcher.log.ErrorContext(ctx, "flushing the certificate transparency counters failed",
			"error", err)
	}
}
