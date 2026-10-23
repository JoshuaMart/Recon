package render

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Politeness bounds per target: a thousand vhosts on one origin are spread
// over time instead of hitting it together.
const (
	perHost   = 2
	perDomain = 4
	// raiseEvery successes add one slot back after a 429 halved them.
	raiseEvery = 20
	// faultPause is how long the dispatcher waits after the renderer failed
	// in a way that is not about the target.
	faultPause = 30 * time.Second
	// pageSize and maxPages bound how far a pass looks down the queue.
	pageSize = 200
	maxPages = 10
	// healthEvery is how often a healthy renderer is asked for its version.
	healthEvery = time.Minute
	// gcEvery is how often unnamed screenshot files are collected.
	gcEvery = time.Hour
)

// Scanner is what the dispatcher needs from the Fingerprinter.
type Scanner interface {
	Scan(ctx context.Context, url string) (*fingerprint.Result, []byte, error)
	Health(ctx context.Context) (string, error)
}

// Dispatcher renders due assets at the pace the Fingerprinter sustains. The
// due date is the queue: nothing moves it but writing a result, so a refused,
// failed or interrupted render stays due and a restart loses nothing.
type Dispatcher struct {
	Writer  *Writer
	Scanner Scanner
	Log     *slog.Logger
	Max     int // the most renders in flight, below the Fingerprinter's own limit
	Tick    time.Duration

	mu         sync.Mutex
	limit      int // current concurrency, halved on a 429
	successes  int
	paused     time.Time
	inflight   map[uuid.UUID]bool
	hosts      map[string]int
	domains    map[string]int
	healthy    bool
	checked    bool // a first health check ran, so a down at start is logged once
	lastHealth time.Time
	wg         sync.WaitGroup
}

// Status is what the health route shows of the renderer.
type Status struct {
	Healthy  bool `json:"healthy"`
	InFlight int  `json:"in_flight"`
	Limit    int  `json:"limit"`
}

// Status reports the dispatcher's state.
func (d *Dispatcher) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{Healthy: d.healthy, InFlight: len(d.inflight), Limit: d.limit}
}

func (d *Dispatcher) init() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight == nil {
		d.inflight, d.hosts, d.domains = map[uuid.UUID]bool{}, map[string]int{}, map[string]int{}
		d.limit = d.Max
	}
}

// Loop dispatches until ctx ends, then waits for the renders in flight.
func (d *Dispatcher) Loop(ctx context.Context) {
	d.init()
	t := time.NewTicker(d.Tick)
	defer t.Stop()
	var lastGC time.Time
	for {
		if err := d.Pass(ctx); err != nil && ctx.Err() == nil {
			d.Log.ErrorContext(ctx, "render dispatcher", "err", err)
		}
		if now := d.Writer.now(); now.Sub(lastGC) >= gcEvery {
			lastGC = now
			n, err := CollectGarbage(ctx, d.Writer.Store.Q, d.Writer.Storage, now)
			switch {
			case err != nil && ctx.Err() == nil:
				d.Log.ErrorContext(ctx, "screenshot garbage collection", "err", err)
			case n > 0:
				d.Log.InfoContext(ctx, "screenshot files collected", "deleted", n)
			}
		}
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Pass checks the renderer, then starts as many due renders as the limits
// allow. Exported for tests, which drive passes themselves.
func (d *Dispatcher) Pass(ctx context.Context) error {
	d.init()
	now := d.Writer.now()
	d.mu.Lock()
	paused := now.Before(d.paused)
	d.mu.Unlock()
	if paused {
		return nil
	}
	if d.healthDue(now) {
		if err := d.checkHealth(ctx, now); err != nil {
			return nil //nolint:nilerr // a down renderer is a state, logged once by checkHealth
		}
	}

	d.mu.Lock()
	free := d.limit - len(d.inflight)
	d.mu.Unlock()
	// Page through the queue until the free slots are filled: a head of the
	// queue held back by politeness (a thousand vhosts of one domain) must not
	// keep the slots idle while other targets wait behind it.
	for page := 0; free > 0 && page < maxPages; page++ {
		due, err := d.Writer.Store.Q.DueRenders(ctx, sqlcgen.DueRendersParams{
			Now: now, Lim: pageSize, Skip: int32(page) * pageSize, //nolint:gosec // bounded by maxPages
		})
		if err != nil {
			return err
		}
		for _, r := range due {
			if free == 0 {
				break
			}
			t := Target{
				AssetID: r.AssetID, ProgramID: r.ProgramID, Kind: r.Kind, Key: r.Key, Host: r.Host,
				Port: int(deref(r.Port)), Scheme: derefS(r.Scheme),
			}
			if !d.claim(t) {
				continue
			}
			free--
			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				defer d.release(t)
				d.render(ctx, t)
			}()
		}
		if len(due) < pageSize {
			break
		}
	}
	return nil
}

// healthDue says whether to ask the renderer how it is: once a minute while
// it answers, every pass while it does not, so renders resume promptly.
func (d *Dispatcher) healthDue(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.healthy && now.Sub(d.lastHealth) < healthEvery {
		return false
	}
	d.lastHealth = now
	return true
}

func domainOf(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

func (d *Dispatcher) claim(t Target) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	dom := domainOf(t.Host)
	if d.inflight[t.AssetID] || d.hosts[t.Host] >= perHost || d.domains[dom] >= perDomain {
		return false
	}
	d.inflight[t.AssetID] = true
	d.hosts[t.Host]++
	d.domains[dom]++
	return true
}

func (d *Dispatcher) release(t Target) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, t.AssetID)
	d.hosts[t.Host]--
	d.domains[domainOf(t.Host)]--
}

func (d *Dispatcher) render(ctx context.Context, t Target) {
	res, _, err := d.Scanner.Scan(ctx, t.URL())
	var busy *fingerprint.BusyError
	switch {
	case errors.As(err, &busy):
		// The service was busy: nothing is written, the asset stays due, and
		// the refused callers do not come back together.
		d.mu.Lock()
		// Renders refused together are one congestion event: halve once,
		// not once per refused call.
		if now := d.Writer.now(); !now.Before(d.paused) {
			d.limit = max(1, d.limit/2)
			d.successes = 0
			wait := time.Duration(float64(busy.RetryAfter) * (0.5 + rand.Float64())) //nolint:gosec // jitter
			d.paused = now.Add(wait)
		}
		d.mu.Unlock()
		return
	case errors.Is(err, fingerprint.ErrUnreachable):
		if err := d.Writer.Unreachable(context.WithoutCancel(ctx), t, "no_response"); err != nil {
			d.Log.ErrorContext(ctx, "render: writing an unreachable target", "asset", t.AssetID, "err", err)
		}
		return
	case err != nil:
		if ctx.Err() == nil {
			d.Log.WarnContext(ctx, "render failed, pausing", "url", t.URL(), "err", err)
			d.mu.Lock()
			d.paused = d.Writer.now().Add(faultPause)
			d.mu.Unlock()
		}
		return
	}
	out, err := d.Writer.Result(context.WithoutCancel(ctx), t, res)
	if err != nil {
		d.Log.ErrorContext(ctx, "render: writing the result", "asset", t.AssetID, "err", err)
		return
	}
	if out.ScreenshotError != nil {
		d.Log.WarnContext(ctx, "render: unusable screenshot, render kept without it", "url", t.URL(), "err", out.ScreenshotError)
	}
	d.mu.Lock()
	d.successes++
	if d.successes%raiseEvery == 0 && d.limit < d.Max {
		d.limit++
	}
	d.mu.Unlock()
}

// checkHealth reads the renderer's version: down pauses everything, a new
// major makes everything due again.
func (d *Dispatcher) checkHealth(ctx context.Context, now time.Time) error {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	version, err := d.Scanner.Health(hctx)
	d.mu.Lock()
	// Logged on the transition only, not on every pass while it stays down.
	report := err != nil && (d.healthy || !d.checked)
	d.healthy, d.checked = err == nil, true
	d.mu.Unlock()
	if err != nil {
		if report {
			d.Log.WarnContext(ctx, "fingerprinter unreachable, renders paused", "err", err)
		}
		d.mu.Lock()
		d.paused = now.Add(faultPause)
		d.mu.Unlock()
		return err
	}
	q := d.Writer.Store.Q
	stored, err := q.GetRenderVersion(ctx)
	if err != nil {
		return err
	}
	if stored == nil || *stored != version {
		was, okWas := 0, false
		if stored != nil {
			was, okWas = fingerprint.Major(*stored)
		}
		is, okIs := fingerprint.Major(version)
		if okWas && okIs && was != is {
			n, err := q.RerenderAll(ctx, now)
			if err != nil {
				return err
			}
			d.Log.InfoContext(ctx, "fingerprinter major version changed, everything due again", "from", *stored, "to", version, "assets", n)
		}
		if err := q.SetRenderVersion(ctx, &version); err != nil {
			return err
		}
	}
	return nil
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
