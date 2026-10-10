package render

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxRefusals is how many saturations end a pass. A saturated service refuses
// the next four hundred targets too, so knocking is wasted effort. Stopping
// costs nothing: due dates were never moved, so refused assets are still due.
const maxRefusals = 3

// censusInterval is how often the unobservable count is taken. It groups over
// the whole projection and only moves with observations, so every tick would be
// a full scan for nothing.
const censusInterval = 5 * time.Minute

// Pass renders what is due.
type Pass struct {
	pool     *pgxpool.Pool
	client   *fingerprint.Client
	ingestor *ingest.Ingestor
	budget   *Budget
	batch    int
	// concurrency is set above where the budget binds, so a programme is throttled
	// by its published rate limit and not an uncalibrated number.
	concurrency int
	// unobservableAlert is the share of a programme's inventory that turns a mass
	// tip into an alert instead of a row nobody reads.
	unobservableAlert float64
	// maxWait is how long a render waits for a programme's budget. Past it the pass
	// stops; due dates never moved, so the rest is still due next tick.
	maxWait time.Duration
	// blind is how long a target nothing could render waits: the cadence of the
	// regime where neither observer gets through. Nothing was learned, so something
	// has to keep asking, not often.
	blind time.Duration
	// lastCensus throttles the unobservable count, a full-projection grouping that
	// moves only with observations.
	censusMu   sync.Mutex
	lastCensus time.Time
	sleep      func(context.Context, time.Duration) bool
	now        func() time.Time
	log        *slog.Logger
}

// Options configure a pass.
type Options struct {
	Batch             int
	Concurrency       int
	UnobservableAlert float64
	MaxWait           time.Duration
	Blind             time.Duration
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) bool
}

// New builds a pass.
func New(pool *pgxpool.Pool, client *fingerprint.Client, ingestor *ingest.Ingestor, budget *Budget, opts Options, log *slog.Logger) *Pass {
	if opts.Batch <= 0 {
		opts.Batch = 200
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.UnobservableAlert <= 0 {
		opts.UnobservableAlert = 0.2
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = 10 * time.Second
	}
	if opts.Blind <= 0 {
		opts.Blind = lifecycle.DefaultCadence().RenderBlind
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	return &Pass{
		pool: pool, client: client, ingestor: ingestor, budget: budget,
		batch: opts.Batch, concurrency: opts.Concurrency,
		unobservableAlert: opts.UnobservableAlert,
		maxWait:           opts.MaxWait,
		blind:             opts.Blind,
		now:               opts.Now, sleep: opts.Sleep, log: log,
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Summary is what one pass did.
type Summary struct {
	Selected  int
	Rendered  int
	Pages     int
	Refused   int
	Skipped   int
	Failed    int
	Saturated bool
	Starved   bool
}

// Once runs a single pass. It ends in one of three ways, none writing anything:
// the selection is empty, a programme's budget refuses to wait, or the service
// refuses often enough to say it is busy.
func (p *Pass) Once(ctx context.Context) (Summary, error) {
	var summary Summary

	queries := sqlcgen.New(p.pool)
	due, err := queries.SelectDueRenders(ctx, sqlcgen.SelectDueRendersParams{
		At:        stamp(p.now()),
		BatchSize: bounded(p.batch),
	})
	if err != nil {
		return summary, fmt.Errorf("select due renders: %w", err)
	}
	summary.Selected = len(due)
	if len(due) == 0 {
		return summary, nil
	}

	var (
		mu       sync.Mutex
		refusals int
		stop     bool
		// Starvation belongs to a programme, not a pass. Workers on one programme compute
		// the same wait, wake together, and exactly one wins the retry. If the losers
		// stopped the pass, a one-programme deployment would render two assets a tick
		// whatever its batch size, and other programmes' work would be dropped too.
		starvedPrograms = map[uuid.UUID]struct{}{}
	)
	work := make(chan sqlcgen.SelectDueRendersRow)
	var wg sync.WaitGroup

	for range p.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range work {
				program := uuid.UUID(row.ProgramID.Bytes)

				mu.Lock()
				halted := stop
				_, dry := starvedPrograms[program]
				mu.Unlock()
				if halted || ctx.Err() != nil {
					continue
				}
				if dry {
					mu.Lock()
					summary.Skipped++
					mu.Unlock()
					continue
				}

				result := p.render(ctx, row)

				mu.Lock()
				switch result.kind {
				case rendered:
					summary.Rendered++
					if result.page {
						summary.Pages++
					}
				case saturated:
					summary.Refused++
					summary.Saturated = true
					refusals++
					if refusals >= maxRefusals {
						stop = true
					}
				case starved:
					// This programme is done for the pass; no other is touched. Nothing was
					// written, so what it did not reach is still due next tick.
					summary.Starved = true
					summary.Skipped++
					starvedPrograms[program] = struct{}{}
				case skipped:
					summary.Skipped++
				default:
					summary.Failed++
				}
				wait := result.wait
				mu.Unlock()

				if wait > 0 {
					p.sleep(ctx, wait)
				}
			}
		}()
	}

	for _, row := range due {
		work <- row
	}
	close(work)
	wg.Wait()

	p.alertUnobservable(ctx, queries)
	return summary, nil
}

type outcomeKind int

const (
	failed outcomeKind = iota
	rendered
	saturated
	starved
	skipped
)

type attempt struct {
	kind outcomeKind
	page bool
	wait time.Duration
}

func (p *Pass) render(ctx context.Context, row sqlcgen.SelectDueRendersRow) attempt {
	program := uuid.UUID(row.ProgramID.Bytes)

	target, ok := renderURL(row)
	if !ok {
		// Nothing to point a browser at: not an error or a measurement, the asset is not
		// a web surface. It must still leave the queue or it heads every batch forever.
		p.log.WarnContext(ctx, "no render target", "asset", row.Key)
		p.backOff(ctx, row)
		return attempt{kind: skipped}
	}

	// A render takes seconds and the budget refills meanwhile, so a short wait is ordinary; only
	// a wait long enough to say the programme's rate cannot support a render ends the pass.
	//
	// Waited for in a loop, as one retry was not enough: every worker on a programme computes
	// the same wait and wakes together, and the bucket holds at most one render, so one wins and
	// the rest reported starvation. With a single programme that ended the pass (selected=28,
	// rendered=0, skipped=26, starved=true every tick, at a rate affording a render every 0.3 s,
	// with an idle service and a queue that never drained). Losing a race to a sibling is the
	// budget working, so it is waited out.
	//
	// Bounded in total, not per attempt: a programme that truly cannot feed this worker still
	// ends its share instead of holding a slot while rows keep arriving.
	deadline := p.now().Add(p.maxWait)
	for {
		taken, wait := p.budget.Reserve(program, int(row.RateLimitRps))
		if taken {
			break
		}
		if wait > p.maxWait || !p.now().Before(deadline) {
			return attempt{kind: starved}
		}
		if !p.sleep(ctx, wait) {
			return attempt{kind: starved}
		}
	}

	result, err := p.client.Scan(ctx, target, fingerprint.Options{})
	if err != nil {
		var busy *fingerprint.Saturated
		switch {
		case errors.As(err, &busy):
			// A state of the service, so the asset is untouched: no observation, counter,
			// streak or timestamp. The charge comes back as nothing reached the target.
			p.budget.Refund(program)
			return attempt{kind: saturated, wait: busy.RetryAfter}
		case errors.Is(err, fingerprint.ErrInternal):
			p.budget.Refund(program)
			p.log.ErrorContext(ctx, "render refused a target inside an internal range",
				"asset", row.Key, "url", target, "error", err)
			p.backOff(ctx, row)
			return attempt{kind: skipped}
		default:
			// The service could not address the target. Also a probe error (no observation
			// or counter), but the attempt must still widen or the asset is retried every
			// minute.
			p.budget.Refund(program)
			p.log.WarnContext(ctx, "render failed", "asset", row.Key, "error", err)
			p.backOff(ctx, row)
			return attempt{kind: failed}
		}
	}

	if err := p.write(ctx, row, target, result); err != nil {
		p.log.ErrorContext(ctx, "render not written", "asset", row.Key, "error", err)
		return attempt{kind: failed}
	}
	_, page := result.Final()
	return attempt{kind: rendered, page: page}
}

// backOff moves a render out without writing anything about the target.
//
// A probe error is not a measurement: nothing is observed and no counter or
// timestamp moves, because nothing was learned. The due date must move though,
// since it is the queue and an unrenderable asset would otherwise head it
// forever. Saturation is left alone on purpose: it is a state of the service that
// clears in seconds, and the pass stops after three refusals anyway.
func (p *Pass) backOff(ctx context.Context, row sqlcgen.SelectDueRendersRow) {
	next := p.now().Add(p.blind)
	if err := sqlcgen.New(p.pool).RescheduleRender(ctx, sqlcgen.RescheduleRenderParams{
		AssetID:  row.AssetID,
		At:       stamp(next),
		Priority: lifecycle.PriorityBaseline,
	}); err != nil {
		p.log.ErrorContext(ctx, "render back-off failed", "asset", row.Key, "error", err)
	}
}

func (p *Pass) write(ctx context.Context, row sqlcgen.SelectDueRendersRow, target string, result *fingerprint.Result) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	asset := ingest.RenderTarget{
		AssetID:   uuid.UUID(row.AssetID.Bytes),
		OrgID:     uuid.UUID(row.OrgID.Bytes),
		ProgramID: uuid.UUID(row.ProgramID.Bytes),
		Kind:      row.Kind,
		Key:       row.Key,
		URL:       target,
	}
	// Carried, not left zero: a field that looks like the asset's edge and never is
	// a lie the next reader has to discover.
	if row.IsCdn != nil {
		asset.Fronted = *row.IsCdn
	}
	if row.CdnProvider != nil {
		asset.Provider = *row.CdnProvider
	}

	written, err := p.ingestor.Render(ctx, sqlcgen.New(tx), asset, result)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	p.log.InfoContext(ctx, "rendered",
		"asset", row.Key, "url", target, "outcome", written.Outcome,
		"usable", written.Usable, "page", written.Page,
		"lifecycle", written.Lifecycle, "next", written.Next)
	return nil
}

// alertUnobservable reports a programme tipping wholesale. That differs from one
// asset going quiet and usually says something about the observer: a banned
// address, a broken egress, a renderer no longer clearing challenges. For now it
// is an error-level log, the ops signal.
func (p *Pass) alertUnobservable(ctx context.Context, queries *sqlcgen.Queries) {
	p.censusMu.Lock()
	now := p.now()
	if !p.lastCensus.IsZero() && now.Sub(p.lastCensus) < censusInterval {
		p.censusMu.Unlock()
		return
	}
	p.lastCensus = now
	p.censusMu.Unlock()

	rows, err := queries.CountUnobservable(ctx)
	if err != nil {
		// The window goes back, or a failing census silences the alert for as long as it
		// keeps failing.
		p.censusMu.Lock()
		p.lastCensus = time.Time{}
		p.censusMu.Unlock()
		p.log.ErrorContext(ctx, "unobservable census failed", "error", err)
		return
	}
	for _, row := range rows {
		if row.Total == 0 {
			continue
		}
		share := float64(row.Unobservable) / float64(row.Total)
		if share < p.unobservableAlert {
			continue
		}
		p.log.ErrorContext(ctx, "a programme has tipped into unobservable",
			"program", uuid.UUID(row.ProgramID.Bytes), "name", row.Name,
			"unobservable", row.Unobservable, "total", row.Total,
			"share", share, "threshold", p.unobservableAlert)
	}
}

// renderURL is what a browser is pointed at. A declared URL is rendered as
// written, since someone named that path; anything else at the service root, as
// a path a scan landed on describes a redirect, not a surface.
func renderURL(row sqlcgen.SelectDueRendersRow) (string, bool) {
	if row.Kind == "url" {
		return row.Key, true
	}
	if row.Host == nil || *row.Host == "" || row.Port == nil {
		return "", false
	}
	port := int(*row.Port)
	if !fingerprint.Renderable(port) {
		return "", false
	}

	scheme := "http"
	if row.Scheme != nil && *row.Scheme != "" {
		scheme = *row.Scheme
	} else if port == 443 || port == 8443 || port == 9443 {
		scheme = "https"
	}

	host := *row.Host
	authority := host
	if (scheme == "https" && port != 443) || (scheme == "http" && port != 80) {
		authority = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return (&url.URL{Scheme: scheme, Host: authority, Path: "/"}).String(), true
}

// bounded narrows a batch size to its column. The setting is validated; the
// clamp covers unvalidated ones becoming a negative limit.
func bounded(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 1 {
		return 1
	}
	return int32(n)
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
