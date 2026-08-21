package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Loop ticks the notifier.
type Loop struct {
	notifier *Notifier
	interval time.Duration
	// channel is the configured output, retried until it is settled: the
	// organization is created by a command outside this process, so a bootstrap
	// that only ran at startup would leave the channel missing until somebody
	// restarted for an unrelated reason.
	channel ConfigChannel
	settled bool
	// unobservableAlert is the share of a programme's inventory that makes a
	// mass tip an event rather than a number.
	unobservableAlert float64
}

// ConfigChannel is the output a deployment configures.
type ConfigChannel struct {
	URL         string
	Template    string
	MinPriority string
}

// NewLoop builds it.
func NewLoop(notifier *Notifier, interval time.Duration, unobservableAlert float64, channel ConfigChannel) *Loop {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if unobservableAlert <= 0 {
		unobservableAlert = UnobservableTiers[0]
	}
	return &Loop{
		notifier: notifier, interval: interval,
		unobservableAlert: unobservableAlert, channel: channel,
	}
}

// Run ticks until the context ends.
func (l *Loop) Run(ctx context.Context) {
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

// Once produces the aggregate events and then drains.
//
// The order matters: a mass tip written on this tick has to leave on this tick.
// A takeover is notified with at most one tick between being written and being
// sent, and a programme going dark deserves no worse.
func (l *Loop) Once(ctx context.Context) {
	if !l.settled {
		settled, err := Bootstrap(ctx, sqlcgen.New(l.notifier.pool),
			l.channel.URL, l.channel.Template, l.channel.MinPriority, l.notifier.log)
		if err != nil {
			l.notifier.log.ErrorContext(ctx, "channel bootstrap failed", "error", err)
		}
		l.settled = settled
	}

	if err := l.notifier.Aggregates(ctx, l.unobservableAlert); err != nil {
		l.notifier.log.ErrorContext(ctx, "aggregate events failed", "error", err)
	}

	summary, err := l.notifier.Once(ctx)
	if err != nil {
		l.notifier.log.ErrorContext(ctx, "notifier tick failed", "error", err)
		return
	}
	if summary.Read == 0 {
		return
	}
	l.notifier.log.InfoContext(ctx, "notifier tick",
		"read", summary.Read, "sent", summary.Sent, "suppressed", summary.Suppressed,
		"summarised", summary.Summarised, "failed", summary.Failed, "nowhere", summary.Nowhere)
}

// Aggregates writes the events no single observation can settle.
//
// The mass tip is the case that shows the boundary. A per programme ratio is a
// proportion over an inventory, not a conclusion drawn from one observation,
// and an aggregate is exactly what a sweep is the right tool for. It is the
// only thing given to one.
func (n *Notifier) Aggregates(ctx context.Context, threshold float64) error {
	queries := sqlcgen.New(n.pool)

	now := n.now()
	if err := n.onboarding(ctx, queries, now); err != nil {
		return err
	}

	rows, err := queries.CountUnobservable(ctx)
	if err != nil {
		return fmt.Errorf("count unobservable: %w", err)
	}

	for _, row := range rows {
		if row.Total == 0 {
			continue
		}
		ratio := float64(row.Unobservable) / float64(row.Total)
		if ratio < threshold {
			continue
		}

		program := uuid.UUID(row.ProgramID.Bytes)
		last := n.tips[program]
		speaks, tier := Speaks(ratio, last.tier, last.at, now)
		if !speaks {
			continue
		}

		payload, err := jsonPayload(map[string]any{
			"summary": fmt.Sprintf("%d of %d assets unobservable (%.0f%%)",
				row.Unobservable, row.Total, ratio*100),
			"unobservable": row.Unobservable,
			"total":        row.Total,
			"ratio":        ratio,
			"tier":         UnobservableTiers[tier],
		})
		if err != nil {
			return err
		}

		if _, err := queries.WriteEvents(ctx, []sqlcgen.WriteEventsParams{{
			OrgID:     row.OrgID,
			ProgramID: row.ProgramID,
			Kind:      KindUnobservable,
			Priority:  Priorities[KindUnobservable],
			Payload:   payload,
			CreatedAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true},
		}}); err != nil {
			return fmt.Errorf("write the mass tip of %s: %w", program, err)
		}
		n.tips[program] = tip{tier: tier, at: now}

		n.log.WarnContext(ctx, "programme tipped into unobservable",
			"program", program, "name", row.Name,
			"unobservable", row.Unobservable, "total", row.Total, "ratio", ratio)
	}
	return nil
}

// onboarding emits the summary a first run's silence owes.
//
// The grace holds back the flood, and holding it back without saying so is the
// same loss of signal the cap exists to avoid. The summary goes out once the
// grace has ended, carries the count the grace held, and is a programme event,
// so it escapes the windows: folding an aggregate into a second aggregate
// counts it twice and loses it.
func (n *Notifier) onboarding(ctx context.Context, q *sqlcgen.Queries, now time.Time) error {
	due, err := q.OnboardingDue(ctx)
	if err != nil {
		return fmt.Errorf("read the onboarding summaries: %w", err)
	}

	for _, row := range due {
		grace := Grace{
			CompletedDiscovery: row.CompletedDiscovery,
			AnyDiscovery:       row.AnyDiscovery,
			Assets:             int(row.Assets),
			CreatedAt:          row.CreatedAt.Time,
		}
		if grace.Active(now) {
			continue
		}

		payload := map[string]any{
			"summary": fmt.Sprintf("%d new active assets on %s", row.Held, row.Name),
			"held":    row.Held,
			"reason":  "first run",
		}
		// A programme whose first run never finished is an incident, and
		// absorbing it in silence is a perimeter quietly ceasing to be scanned.
		incident := !row.CompletedDiscovery
		if incident {
			payload["incident"] = "the first discovery run never completed"
		}
		encoded, err := jsonPayload(payload)
		if err != nil {
			return err
		}

		events := []sqlcgen.WriteEventsParams{{
			OrgID:     row.OrgID,
			ProgramID: row.ProgramID,
			Kind:      KindDigest,
			Priority:  Priorities[KindDigest],
			Payload:   encoded,
			CreatedAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true},
		}}
		if incident {
			events = append(events, sqlcgen.WriteEventsParams{
				OrgID:     row.OrgID,
				ProgramID: row.ProgramID,
				Kind:      KindRunNeverEnded,
				Priority:  Priorities[KindRunNeverEnded],
				Payload:   encoded,
				CreatedAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true},
			})
		}
		if _, err := q.WriteEvents(ctx, events); err != nil {
			return fmt.Errorf("write the onboarding summary of %s: %w", row.Name, err)
		}

		n.log.InfoContext(ctx, "onboarding summarised",
			"program", uuid.UUID(row.ProgramID.Bytes), "name", row.Name,
			"held", row.Held, "incident", incident)
	}
	return nil
}
