package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// A Check says whether one component works, and why not when it does not.
type Check struct {
	Component string
	Probe     func(ctx context.Context) (ok bool, detail string)
}

// Watcher announces a component that fails for longer than BrokenAfter, once,
// and its recovery, once. The state is kept in the database so a restart
// neither repeats nor forgets an announcement.
type Watcher struct {
	Pool        *pgxpool.Pool
	Q           *sqlcgen.Queries
	Log         *slog.Logger
	Now         func() time.Time
	Checks      []Check
	BrokenAfter time.Duration
	Tick        time.Duration
}

func (w *Watcher) now() time.Time { return w.Now().UTC().Truncate(time.Microsecond) }

// Loop checks until ctx ends.
func (w *Watcher) Loop(ctx context.Context) {
	t := time.NewTicker(w.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
			w.Log.ErrorContext(ctx, "health pass", "err", err)
		}
	}
}

// Pass runs every check once.
func (w *Watcher) Pass(ctx context.Context) error {
	for _, c := range w.Checks {
		ok, detail := c.Probe(ctx)
		if err := w.record(ctx, c.Component, ok, detail); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) record(ctx context.Context, component string, ok bool, detail string) error {
	now := w.now()
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.Q.WithTx(tx)

	st, err := q.GetHealthState(ctx, component)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	group := "health:" + component
	switch {
	case ok && st.Announced:
		if err := Enqueue(ctx, q, Event{Kind: KindRecovered, Group: group, Title: component + " is back"}, now); err != nil {
			return err
		}
		st = sqlcgen.HealthState{}
	case ok:
		st = sqlcgen.HealthState{}
	case st.FailingSince == nil:
		st.FailingSince = &now
	case !st.Announced && now.Sub(*st.FailingSince) >= w.BrokenAfter:
		title := component + " is failing"
		if detail != "" {
			title += ": " + detail
		}
		if err := Enqueue(ctx, q, Event{Kind: KindBroken, Group: group, Title: title}, now); err != nil {
			return err
		}
		st.Announced = true
	}
	var d *string
	if !ok && detail != "" {
		d = &detail
	}
	if err := q.PutHealthState(ctx, sqlcgen.PutHealthStateParams{
		Component: component, FailingSince: st.FailingSince, Announced: st.Announced, Detail: d,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
