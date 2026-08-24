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

// maxFrameBytes bounds one message.
//
// The feed is a public aggregator carrying certificates anybody can get logged,
// so its output is influenced by somebody else and needs a ceiling like any
// other such input. Measured on the lite stream: median 1.7 kB, p99 2.4 kB,
// widest 7.9 kB over four thousand certificates. A megabyte is two orders of
// magnitude of headroom and still refuses a stream that decided to send a
// gigabyte.
const maxFrameBytes = 1 << 20

// Feed dials the aggregator and drives the matcher with what it sends.
//
// It reconnects, and a reconnection is a gap that nothing backfills: the
// aggregator keeps no history on Recon's behalf, so whatever passed while the
// socket was down is lost and resuming starts at the present. That is why the
// matcher records the minutes it was alive rather than trying to reconstruct
// the ones it was not, and why periodic enumeration is not redundant with this.
type Feed struct {
	url     string
	matcher *Matcher
	log     *slog.Logger

	// Backoff bounds how fast a broken feed is redialled. A dropped socket is
	// usually the aggregator restarting, so the first retry is quick and the
	// ceiling is what stops a dead endpoint being dialled in a tight loop.
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

		// A session that lasted is a working endpoint that dropped, not a
		// broken one: it starts again from the short delay. Without this a feed
		// that reconnects every few hours would creep up to the ceiling and
		// stay there.
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
			// One unreadable message is not a reason to drop a working socket,
			// and it is counted rather than logged per occurrence: at a few
			// thousand frames a second, a log line each would be the outage.
			f.matcher.undecodable()
			continue
		}
		f.matcher.Handle(ctx, &frame)
	}
}

// Loop reloads the apex set and flushes the counters on a tick.
//
// One ticker for both, because they are the same moment by design: the counters
// are accumulated in memory and written when the set is refreshed, which is
// what makes a write per certificate unnecessary.
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

// Run ticks until the context ends.
//
// The first reload happens before the first tick, because a matcher with an
// empty set matches nothing: waiting a whole interval would mean a minute of
// certificates arriving against a perimeter this process has not read yet.
func (l *Loop) Run(ctx context.Context) {
	l.once(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The window in memory is written on the way out, so a clean
			// shutdown loses nothing. A crash still does, which is what the
			// flush being a metric rather than the journal allows.
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
