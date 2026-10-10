// Package maintenance runs the loops that keep the database usable. Nothing here
// is a feature: a partition job inside another component's tick inherits that
// component's toggle, so turning something off reopens an ingestion outage three
// months later.
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

// monthsAhead is how far the job works. Two, so an incident at month end does not
// interrupt ingestion. There is no default partition: a row whose month is
// missing must fail loudly, the only signal that this loop stopped.
const monthsAhead = 2

// Retention is how long each population of events is kept. Asymmetric: alert
// history is what people look back through, suppressed rows are onboarding and
// overflow noise needed for an investigation, and the queue is never purged
// because that loses alerts.
const (
	// SuppressedRetention bounds the noise. A targeted delete, not a partition drop:
	// those rows are written in waves and the delete is bounded.
	SuppressedRetention = 30 * 24 * time.Hour
	// FeedMinuteRetention bounds the CT feed's own record. A row a minute is half a
	// million a year: small but unbounded, and nothing else in the schema grows
	// forever.
	//
	// It is a horizon: an apex watched for a year has a longer span, so the coverage
	// reading clips to this value and says so. Otherwise a healthy feed reads as
	// half down on any older apex.
	FeedMinuteRetention = 180 * 24 * time.Hour
	// StuckAfter is when a queue that is not draining becomes an alert. A broken
	// notifier is silent: ingestion keeps writing and the inventory stays correct,
	// until the takeover it misses.
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

// Run ticks until the context ends. It runs once immediately: a deployment
// starting on the last day of a month must not depend on staying up for an hour.
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

// Once does a single pass, which tests call too.
func (l *Loop) Once(ctx context.Context) {
	for _, table := range partitioned {
		created, err := l.queries.EnsurePartitions(ctx, sqlcgen.EnsurePartitionsParams{
			Target:      table,
			MonthsAhead: monthsAhead,
		})
		if err != nil {
			// Loud, because the failure is silent elsewhere: ingestion works until the month
			// it could not create arrives.
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

// trimFeed bounds the record of when the CT feed was alive. It is here, not in the
// matcher's tick, for the reason this package exists: inside a feature's loop it
// would inherit that toggle, and a deployment without a feed still has a table
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

// purge drops the onboarding and overflow noise. Like the partition job it is
// not a feature: in the notifier's tick it would inherit a notifications toggle,
// and turning alerts off would reopen the ingestion outage.
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
		// Loud, because nothing else announces it: ingestion keeps writing while alerts
		// stop.
		l.log.ErrorContext(ctx, "notifications are not draining",
			"pending", count, "older_than", StuckAfter.String())
	}
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
