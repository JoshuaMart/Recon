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
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxRefusals is how many saturations end a pass.
//
// A saturated service will refuse the next four hundred targets for the same
// reason, so knocking is spent effort. Stopping costs nothing: the due dates
// were never moved, so every refused asset is still due on the next tick, by
// the same ordering that put it there.
const maxRefusals = 3

// censusInterval is how often the unobservable count is taken. The number moves
// only when observations do, and the query groups over the whole projection, so
// taking it on every tick would buy nothing for a full scan a minute.
const censusInterval = 5 * time.Minute

// Pass renders what is due.
type Pass struct {
	pool     *pgxpool.Pool
	client   *fingerprint.Client
	ingestor *ingest.Ingestor
	budget   *Budget
	batch    int
	// concurrency is set above where the budget binds rather than at it, so
	// the thing throttling a programme is its published rate limit and not a
	// number nobody calibrated.
	concurrency int
	// unobservableAlert is the share of a programme's inventory that turns a
	// mass tip into an alert rather than a row nobody reads.
	unobservableAlert float64
	// maxWait is how long a render will wait for a programme's budget. Past it
	// the pass stops: the due dates were never moved, so everything it did not
	// reach is still due on the next tick.
	maxWait time.Duration
	// lastCensus throttles the unobservable count. It groups over the whole
	// projection, so running it on every tick would put a full scan of the
	// inventory on a loop that runs every minute for a number that moves when
	// observations do.
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

// Once runs a single pass.
//
// It ends in one of three ways and none of them writes anything: the selection
// comes back empty, a programme's budget refuses to wait, or the service
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
	)
	work := make(chan sqlcgen.SelectDueRendersRow)
	var wg sync.WaitGroup

	for range p.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range work {
				mu.Lock()
				halted := stop
				mu.Unlock()
				if halted || ctx.Err() != nil {
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
					summary.Starved = true
					summary.Skipped++
					stop = true
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
		// Nothing to point a browser at. It is not an error and not a
		// measurement: the asset simply is not a web surface.
		p.log.WarnContext(ctx, "no render target", "asset", row.Key)
		return attempt{kind: skipped}
	}

	// A render takes seconds and the budget refills while it runs, so a short
	// wait is the ordinary case rather than a failure. What ends a pass is a
	// wait long enough to say the programme's rate cannot support one now.
	taken, wait := p.budget.Reserve(program, int(row.RateLimitRps))
	if !taken {
		if wait > p.maxWait || !p.sleep(ctx, wait) {
			return attempt{kind: starved}
		}
		if taken, _ = p.budget.Reserve(program, int(row.RateLimitRps)); !taken {
			return attempt{kind: starved}
		}
	}

	result, err := p.client.Scan(ctx, target, fingerprint.Options{})
	if err != nil {
		var busy *fingerprint.Saturated
		switch {
		case errors.As(err, &busy):
			// A state of the service, so it must not touch the asset. No
			// observation, no counter, no streak, no timestamp. The charge
			// comes back because nothing reached the target.
			p.budget.Refund(program)
			return attempt{kind: saturated, wait: busy.RetryAfter}
		case errors.Is(err, fingerprint.ErrInternal):
			p.budget.Refund(program)
			p.log.ErrorContext(ctx, "render refused a target inside an internal range",
				"asset", row.Key, "url", target, "error", err)
			return attempt{kind: skipped}
		default:
			// The service could not address the target at all. Also a probe
			// error, and also nothing written.
			p.budget.Refund(program)
			p.log.WarnContext(ctx, "render failed", "asset", row.Key, "error", err)
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

func (p *Pass) write(ctx context.Context, row sqlcgen.SelectDueRendersRow, target string, result *fingerprint.Result) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	written, err := p.ingestor.Render(ctx, sqlcgen.New(tx), ingest.RenderTarget{
		AssetID:   uuid.UUID(row.AssetID.Bytes),
		OrgID:     uuid.UUID(row.OrgID.Bytes),
		ProgramID: uuid.UUID(row.ProgramID.Bytes),
		Kind:      row.Kind,
		Key:       row.Key,
		URL:       target,
	}, result)
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

// alertUnobservable reports a programme tipping wholesale.
//
// A mass tip is a different event from one asset going quiet, and it usually
// says something about the observer rather than about the targets: an address
// that got banned, an egress that broke, a renderer that stopped clearing
// challenges. Until the notification path exists this is a log at error level,
// which is the ops signal; phase 5 turns it into an event.
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

// renderURL is what a browser is pointed at.
//
// A declared URL is rendered as it was written, because somebody named that
// path. Everything else is rendered at the service root: a path a scan landed
// on describes a redirect rather than a surface.
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

// bounded narrows a batch size to the column that holds it. The setting is
// already validated as positive; the clamp is here so that one nobody validated
// cannot turn a batch into a negative limit.
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
