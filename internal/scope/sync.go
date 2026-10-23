package scope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Errors a sync pass can end on without having changed anything.
var (
	ErrRunning    = errors.New("a sync pass is already running")
	ErrSuspicious = errors.New("refused: the answer would archive too many programs")
)

// Summary is what one pass changed.
type Summary struct {
	Programs    int `json:"programs"`
	Created     int `json:"created"`
	Archived    int `json:"archived"`
	Reactivated int `json:"reactivated"`
	RulesOpened int `json:"rules_opened"`
	RulesClosed int `json:"rules_closed"`
	Untracked   int `json:"untracked"`
	// Refreshed counts the programs whose inventory was brought in line with
	// their new rules.
	Refreshed int `json:"refreshed"`
}

// RefreshFunc brings a program's inventory in line with its rules, in the
// sync's transaction. It lives in the inventory package, which depends on this
// one.
type RefreshFunc func(ctx context.Context, q *sqlcgen.Queries, p sqlcgen.Program, now time.Time) error

// Syncer reconciles synced programs with ScopesExtractor.
type Syncer struct {
	Store  *store.Store
	Source Source
	Log    *slog.Logger
	// MaxRemovedRatio is the share of active synced programs one pass may
	// archive. Above it the pass is refused.
	MaxRemovedRatio float64
	Now             func() time.Time
	// Refresh, when set, runs for every program whose rules moved.
	Refresh RefreshFunc
}

type programKey struct{ platform, slug string }

type remoteProgram struct {
	name      string
	bounty    bool
	rules     map[Rule]bool
	untracked []untracked
}

type untracked struct{ value, typ, reason string }

func group(entries []Entry) map[programKey]*remoteProgram {
	out := map[programKey]*remoteProgram{}
	for _, e := range entries {
		k := programKey{strings.ToLower(strings.TrimSpace(e.Platform)), strings.TrimSpace(e.Slug)}
		if k.platform == "" || k.slug == "" {
			continue
		}
		p := out[k]
		if p == nil {
			p = &remoteProgram{name: e.ProgramName, rules: map[Rule]bool{}}
			out[k] = p
		}
		p.bounty = p.bounty || e.Bounty
		if r, reason := FromScopeValue(e.Value, e.Type, e.IsInScope); reason == "" {
			p.rules[r] = true
		} else {
			p.untracked = append(p.untracked, untracked{strings.TrimSpace(e.Value), e.Type, reason})
		}
	}
	for k, p := range out {
		if strings.TrimSpace(p.name) == "" {
			p.name = k.slug
		}
	}
	return out
}

// Run is one pass. It changes nothing unless it can apply the whole answer.
func (s *Syncer) Run(ctx context.Context) (Summary, error) {
	// PostgreSQL keeps microseconds: a finer instant would never compare equal
	// to what it wrote.
	now := s.Now().UTC().Truncate(time.Microsecond)
	if err := s.Store.Q.SyncStarted(ctx, now); err != nil {
		return Summary{}, err
	}
	sum, err := s.run(ctx, now)
	if err != nil {
		if !errors.Is(err, ErrRunning) {
			if ferr := s.Store.Q.SyncFailed(ctx, sqlcgen.SyncFailedParams{Error: err.Error(), Now: now}); ferr != nil {
				s.Log.ErrorContext(ctx, "recording the sync failure", "err", ferr)
			}
		}
		return Summary{}, err
	}
	return sum, nil
}

func (s *Syncer) run(ctx context.Context, now time.Time) (Summary, error) {
	entries, err := s.Source.Scopes(ctx)
	if err != nil {
		return Summary{}, err
	}
	remote := group(entries)

	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)

	locked, err := q.TryLockSync(ctx)
	if err != nil {
		return Summary{}, err
	}
	if !locked {
		return Summary{}, ErrRunning
	}

	existing, err := q.ListSyncedPrograms(ctx)
	if err != nil {
		return Summary{}, err
	}
	var active, gone []sqlcgen.Program
	for _, p := range existing {
		if p.State != "active" {
			continue
		}
		active = append(active, p)
		if remote[programKey{deref(p.Platform), deref(p.Slug)}] == nil {
			gone = append(gone, p)
		}
	}
	if len(gone) > 0 && (len(remote) == 0 || float64(len(gone))/float64(len(active)) > s.MaxRemovedRatio) {
		return Summary{}, fmt.Errorf("%w: %d of %d active programs missing from an answer of %d", ErrSuspicious, len(gone), len(active), len(remote))
	}

	sum := Summary{Programs: len(remote)}
	keys := make([]programKey, 0, len(remote))
	for k := range remote {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b programKey) int { return strings.Compare(a.platform+"/"+a.slug, b.platform+"/"+b.slug) })
	for _, k := range keys {
		if err := s.apply(ctx, q, now, k, remote[k], &sum); err != nil {
			return Summary{}, fmt.Errorf("%s/%s: %w", k.platform, k.slug, err)
		}
	}
	for _, p := range gone {
		sync := "sync"
		if err := q.SetProgramState(ctx, sqlcgen.SetProgramStateParams{ID: p.ID, State: "archived", ArchivedBy: &sync, Now: now}); err != nil {
			return Summary{}, err
		}
		sum.Archived++
	}

	raw, err := json.Marshal(sum)
	if err != nil {
		return Summary{}, err
	}
	if err := q.SyncSucceeded(ctx, sqlcgen.SyncSucceededParams{Now: now, Summary: raw}); err != nil {
		return Summary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

func (s *Syncer) apply(ctx context.Context, q *sqlcgen.Queries, now time.Time, k programKey, rp *remoteProgram, sum *Summary) error {
	p, err := q.UpsertSyncedProgram(ctx, sqlcgen.UpsertSyncedProgramParams{
		Platform: &k.platform, Slug: &k.slug, Name: rp.name, Bounty: &rp.bounty, Now: now,
	})
	if err != nil {
		return err
	}
	if p.CreatedAt.Equal(now) {
		sum.Created++
	}
	if p.State == "archived" && deref(p.ArchivedBy) == "sync" {
		if err := q.SetProgramState(ctx, sqlcgen.SetProgramStateParams{ID: p.ID, State: "active", Now: now}); err != nil {
			return err
		}
		p.State, p.ArchivedBy = "active", nil
		sum.Reactivated++
	}
	moved := sum.RulesOpened + sum.RulesClosed

	open, err := q.ListOpenRulesByOrigin(ctx, sqlcgen.ListOpenRulesByOriginParams{ProgramID: p.ID, Origin: "sync"})
	if err != nil {
		return err
	}
	have := map[Rule]bool{}
	for _, r := range open {
		rule := Rule{r.Kind, r.Matcher, r.Pattern}
		have[rule] = true
		if !rp.rules[rule] {
			if _, err := q.CloseRule(ctx, sqlcgen.CloseRuleParams{ID: r.ID, Now: now}); err != nil {
				return err
			}
			sum.RulesClosed++
		}
	}
	for rule := range rp.rules {
		if have[rule] {
			continue
		}
		if _, err := OpenRule(ctx, q, p.ID, rule, "sync", nil, now); err != nil {
			return err
		}
		sum.RulesOpened++
	}

	for _, u := range rp.untracked {
		if err := q.UpsertUntracked(ctx, sqlcgen.UpsertUntrackedParams{
			ProgramID: p.ID, Value: u.value, Type: u.typ, Reason: u.reason, Now: now,
		}); err != nil {
			return err
		}
	}
	sum.Untracked += len(rp.untracked)
	if err := q.MarkUntrackedGone(ctx, sqlcgen.MarkUntrackedGoneParams{ProgramID: p.ID, Now: now}); err != nil {
		return err
	}
	if s.Refresh != nil && sum.RulesOpened+sum.RulesClosed > moved {
		if err := s.Refresh(ctx, q, p, now); err != nil {
			return fmt.Errorf("refresh: %w", err)
		}
		sum.Refreshed++
	}
	return nil
}

// OpenRule opens a rule and, for an include apex, its wildcard. It returns
// pgx.ErrNoRows when the same rule is already open.
func OpenRule(ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, r Rule, origin string, note *string, now time.Time) (sqlcgen.ScopeRule, error) {
	row, err := q.OpenRule(ctx, sqlcgen.OpenRuleParams{
		ProgramID: programID, Kind: r.Kind, Matcher: r.Matcher, Pattern: r.Pattern, Origin: origin, Note: note, Now: now,
	})
	if err != nil {
		return sqlcgen.ScopeRule{}, err
	}
	if r.Kind == Include && r.Matcher == Apex {
		if err := q.CreateWildcard(ctx, sqlcgen.CreateWildcardParams{ProgramID: programID, RuleID: row.ID, Apex: r.Pattern, Now: now}); err != nil {
			return sqlcgen.ScopeRule{}, err
		}
	}
	return row, nil
}

// Loop runs a pass now and then every interval until ctx ends.
func (s *Syncer) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sum, err := s.Run(ctx)
		switch {
		case err == nil:
			s.Log.InfoContext(ctx, "scope sync", "summary", sum)
		case errors.Is(err, ErrRunning), ctx.Err() != nil:
		default:
			s.Log.ErrorContext(ctx, "scope sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
