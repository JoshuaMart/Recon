// Package maintenance runs the loops that keep the database usable.
//
// Nothing here is a feature, and that is the load-bearing sentence. A partition
// job placed inside another component's tick inherits that component's toggle,
// so turning something off reopens an ingestion outage three months later
// through a button that talks about something else.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// partitioned are the tables that need a month created ahead of time.
var partitioned = []string{"observation", "notification_event"}

// monthsAhead is how far the job works.
//
// Two rather than one, so that an incident at the end of a month does not
// interrupt ingestion while somebody works out what broke. There is no default
// partition to absorb a miss: a row whose month is absent has to fail loudly,
// because that is the only signal this loop has stopped running.
const monthsAhead = 2

// Retention is how long each population of events is kept.
//
// Asymmetric, because the three do not have the same value. The alert history
// is what somebody looks back through; the suppressed rows are onboarding and
// overflow noise, readable for the length of an investigation; and the queue is
// never purged, because purging a queue is losing alerts.
const (
	// SuppressedRetention bounds the noise. A targeted delete inside the
	// partitions rather than a partition drop: those rows are written in waves
	// and the delete is bounded.
	SuppressedRetention = 30 * 24 * time.Hour
	// FeedMinuteRetention bounds the Certificate Transparency feed's own
	// record. One row a minute is half a million a year: small, and unbounded,
	// and unbounded is the half that matters, because nothing else in this
	// schema grows forever. Longer than any coverage reading looks back, since
	// that reading never goes further than an apex has been watched.
	FeedMinuteRetention = 180 * 24 * time.Hour
	// StuckAfter is when a queue that is not draining becomes an alert. A
	// broken notifier is a silent failure by nature: nothing else announces
	// it, ingestion keeps writing and the inventory stays correct, and it is
	// the kind of outage that goes unnoticed until the takeover it misses.
	StuckAfter = 24 * time.Hour
)

// Loop is the housekeeping tick.
type Loop struct {
	queries  *sqlcgen.Queries
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// New builds the loop. It takes no enable flag on purpose.
func New(queries *sqlcgen.Queries, interval time.Duration, log *slog.Logger) *Loop {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Loop{queries: queries, interval: interval, now: time.Now, log: log}
}

// Run ticks until the context ends.
//
// It runs once immediately rather than waiting for the first tick: a deployment
// that starts on the last day of a month must not depend on staying up for an
// hour.
func (l *Loop) Run(ctx context.Context) {
	l.Once(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Once(ctx)
		}
	}
}

// Once does a single pass, which is also what a test can call.
func (l *Loop) Once(ctx context.Context) {
	for _, table := range partitioned {
		created, err := l.queries.EnsurePartitions(ctx, sqlcgen.EnsurePartitionsParams{
			Target:      table,
			MonthsAhead: monthsAhead,
		})
		if err != nil {
			// Loud, because the failure is silent everywhere else: ingestion
			// keeps working until the month it could not create arrives.
			l.log.ErrorContext(ctx, "partition maintenance failed", "table", table, "error", err)
			continue
		}
		if created > 0 {
			l.log.InfoContext(ctx, "partitions created", "table", table, "count", created)
		}
	}

	l.purge(ctx)
	l.trimFeed(ctx)
	l.stuck(ctx)
}

// trimFeed bounds the record of when the Certificate Transparency feed was
// alive. It is here rather than in the matcher's own tick for the reason this
// package exists: a housekeeping job inside a feature's loop inherits that
// feature's toggle, and a deployment with no feed configured still has a table
// that grew while it had one.
func (l *Loop) trimFeed(ctx context.Context) {
	deleted, err := l.queries.PurgeFeedMinutes(ctx, sqlcgen.PurgeFeedMinutesParams{
		Before: stamp(l.now().Add(-FeedMinuteRetention)),
	})
	if err != nil {
		l.log.ErrorContext(ctx, "feed minute purge failed", "error", err)
		return
	}
	if deleted > 0 {
		l.log.InfoContext(ctx, "feed minutes purged", "count", deleted)
	}
}

// purge drops the onboarding and overflow noise.
//
// Neither this nor the partition job is a feature. Placed inside the notifier's
// tick they would inherit a notifications toggle, and turning alerts off would
// reopen an ingestion outage three months later through a button that talks
// about alerts.
func (l *Loop) purge(ctx context.Context) {
	deleted, err := l.queries.PurgeSuppressed(ctx, sqlcgen.PurgeSuppressedParams{
		Before: stamp(l.now().Add(-SuppressedRetention)),
	})
	if err != nil {
		l.log.ErrorContext(ctx, "suppressed purge failed", "error", err)
		return
	}
	if deleted > 0 {
		l.log.InfoContext(ctx, "suppressed events purged", "count", deleted)
	}
}

// stuck reports a queue that is not draining.
func (l *Loop) stuck(ctx context.Context) {
	count, err := l.queries.StuckEvents(ctx, sqlcgen.StuckEventsParams{
		Before: stamp(l.now().Add(-StuckAfter)),
	})
	if err != nil {
		l.log.ErrorContext(ctx, "stuck queue check failed", "error", err)
		return
	}
	if count > 0 {
		// Loud, because nothing else announces it: ingestion keeps writing and
		// the inventory stays correct while the alerts stop.
		l.log.ErrorContext(ctx, "notifications are not draining",
			"pending", count, "older_than", StuckAfter.String())
	}
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
