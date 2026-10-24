package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Evaluator keeps the matches of alerting saved searches and notifies the
// assets that enter one.
type Evaluator struct {
	Pool *pgxpool.Pool
	Q    *sqlcgen.Queries
	Log  *slog.Logger
	Now  func() time.Time
	// PublicURL is where a message's link points.
	PublicURL string
	// Every is the timer pass, for queries whose result moves with time alone.
	Every time.Duration
	// Settle is how long a kick waits for the next ones: an ingestion and the
	// renders it triggers make one evaluation.
	Settle time.Duration

	once sync.Once
	kick chan struct{}
}

func (e *Evaluator) now() time.Time { return e.Now().UTC().Truncate(time.Microsecond) }

// Kick asks for an evaluation soon: something was ingested. It never blocks.
func (e *Evaluator) Kick() {
	select {
	case e.kicks() <- struct{}{}:
	default:
	}
}

func (e *Evaluator) kicks() chan struct{} {
	e.once.Do(func() { e.kick = make(chan struct{}, 1) })
	return e.kick
}

// Loop evaluates on each kick, after it settles, and on the timer.
func (e *Evaluator) Loop(ctx context.Context) {
	kicks := e.kicks()
	t := time.NewTicker(e.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kicks:
			select {
			case <-ctx.Done():
				return
			case <-time.After(e.Settle):
			}
		}
		if err := e.Pass(ctx); err != nil && ctx.Err() == nil {
			e.Log.ErrorContext(ctx, "saved search evaluation", "err", err)
		}
	}
}

// Pass evaluates every alerting search. One that fails, a query the language
// stopped accepting say, does not stop the others.
func (e *Evaluator) Pass(ctx context.Context) error {
	searches, err := e.Q.AlertingSearches(ctx)
	if err != nil {
		return err
	}
	for _, s := range searches {
		if err := e.evaluate(ctx, s); err != nil {
			e.Log.WarnContext(ctx, "saved search not evaluated", "search", s.Name, "err", err)
		}
	}
	return nil
}

func (e *Evaluator) evaluate(ctx context.Context, s sqlcgen.SavedSearch) error {
	now := e.now()
	tx, err := e.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := e.Q.WithTx(tx)

	ids, err := search.Matches(ctx, tx, search.Request{Query: s.Query, View: s.View, Now: now})
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	added, err := q.AddMatches(ctx, sqlcgen.AddMatchesParams{SearchID: s.ID, Assets: ids, Now: now})
	if err != nil {
		return err
	}
	if err := q.DropMatches(ctx, sqlcgen.DropMatchesParams{SearchID: s.ID, Assets: ids}); err != nil {
		return err
	}
	// The first evaluation records what the search already matches: the
	// alert is about what comes next.
	if s.EvaluatedAt != nil && len(added) > 0 {
		labels, err := q.MatchLabels(ctx, added[:min(len(added), MaxItems)])
		if err != nil {
			return err
		}
		items := make([]string, 0, len(labels))
		for _, l := range labels {
			items = append(items, label(l))
		}
		if err := Enqueue(ctx, q, Event{
			Kind: KindSavedSearch, Group: "search:" + s.ID.String(), Title: s.Name, Items: items, Count: len(added),
			Link: e.searchLink(s), Window: SavedSearchWindow,
		}, now); err != nil {
			return err
		}
	}
	if err := q.SearchEvaluated(ctx, sqlcgen.SearchEvaluatedParams{ID: s.ID, Now: now}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *Evaluator) searchLink(s sqlcgen.SavedSearch) string {
	v := url.Values{}
	if s.Query != "" {
		v.Set("q", s.Query)
	}
	if s.View != search.ViewServices {
		v.Set("view", s.View)
	}
	link := strings.TrimRight(e.PublicURL, "/") + "/"
	if len(v) > 0 {
		link += "?" + v.Encode()
	}
	return link
}

// label is how a match is listed: its key, status and title.
func label(l sqlcgen.MatchLabelsRow) string {
	out := strings.TrimSuffix(l.Key, "/tcp")
	if l.StatusCode != nil {
		out += fmt.Sprintf(" · %d", *l.StatusCode)
	}
	if l.Title != nil && *l.Title != "" {
		t := *l.Title
		if r := []rune(t); len(r) > 60 {
			t = string(r[:59]) + "…"
		}
		out += " · " + t
	}
	return out
}
