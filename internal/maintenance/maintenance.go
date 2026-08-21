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

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// partitioned are the tables that need a month created ahead of time.
var partitioned = []string{"observation"}

// monthsAhead is how far the job works.
//
// Two rather than one, so that an incident at the end of a month does not
// interrupt ingestion while somebody works out what broke. There is no default
// partition to absorb a miss: a row whose month is absent has to fail loudly,
// because that is the only signal this loop has stopped running.
const monthsAhead = 2

// Loop is the housekeeping tick.
type Loop struct {
	queries  *sqlcgen.Queries
	interval time.Duration
	log      *slog.Logger
}

// New builds the loop. It takes no enable flag on purpose.
func New(queries *sqlcgen.Queries, interval time.Duration, log *slog.Logger) *Loop {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Loop{queries: queries, interval: interval, log: log}
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
}
