// Package render turns due assets into browser renders.
//
// The queue is a predicate rather than a list, re-evaluated on every tick, and
// a render has no lease: the due date is the queue. That is what makes the path
// idempotent, and it is why nothing here has a reservation to expire or a
// crash window between a refusal and a retry.
package render

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultCost is what one render is charged against a programme's rate limit.
//
// A render does not cost one request. A browser fetches the page, then its same
// host subresources, the scripts, the stylesheets, the images, the XHRs the
// page issues, then the 404 probe, robots.txt, the sitemap and the favicon.
// Thirty is the order of magnitude of a real application page. Third party
// subresources go elsewhere and cost the target nothing, which puts this below
// the browser's total request count and makes it a setting rather than a
// constant.
//
// Billing it as one would make the most expensive thing in the system the
// cheapest on the counter.
const DefaultCost = 30

// Budget meters renders against each programme's published rate limit.
//
// It is in memory, and that assumes one control plane process. A second one
// needs a shared bucket, and that is the condition under which a token bucket
// store comes back into the deployment rather than a thing to build now.
type Budget struct {
	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
	cost    int
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewBudget builds a meter.
func NewBudget(cost int, now func() time.Time) *Budget {
	if cost <= 0 {
		cost = DefaultCost
	}
	if now == nil {
		now = time.Now
	}
	return &Budget{buckets: map[uuid.UUID]*bucket{}, cost: cost, now: now}
}

// Cost is what one render is charged.
func (b *Budget) Cost() int { return b.cost }

// Charge takes a render's worth of budget, or reports that there is none.
//
// The charge happens **before** the call. Charging after would let a burst
// reach the target before anything counted it, which is the one ordering that
// makes a rate limit decorative.
func (b *Budget) Charge(program uuid.UUID, rps int) bool {
	taken, _ := b.Reserve(program, rps)
	return taken
}

// Reserve takes a render's worth, or says how long until there is one.
//
// The wait is the useful half. A render takes seconds and tokens accrue while
// it runs, so a programme at ten requests a second affords one render every
// three seconds and four or five end up in flight at once. A caller that only
// ever saw a refusal would render one asset per pass and stop, which is the
// budget binding on the pass rather than on the target.
func (b *Budget) Reserve(program uuid.UUID, rps int) (bool, time.Duration) {
	if rps <= 0 {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	held, ok := b.buckets[program]
	if !ok {
		// A programme starts with one render's worth rather than a full
		// second's: a burst allowance measured in seconds lets the first pass
		// of a large perimeter go straight past the limit it was given.
		held = &bucket{tokens: float64(b.cost), last: now}
		b.buckets[program] = held
	}

	held.tokens += now.Sub(held.last).Seconds() * float64(rps)
	held.last = now
	// The ceiling is one render, so an idle programme does not accumulate a
	// burst it can spend all at once on somebody's server.
	if ceiling := float64(b.cost); held.tokens > ceiling {
		held.tokens = ceiling
	}

	if held.tokens < float64(b.cost) {
		missing := float64(b.cost) - held.tokens
		return false, time.Duration(missing / float64(rps) * float64(time.Second))
	}
	held.tokens -= float64(b.cost)
	return true, 0
}

// Refund gives a charge back.
//
// A saturated service means nothing reached the target, so the charge returns.
// Without this a busy renderer would throttle a programme's real renders on
// behalf of renders that never happened, which is the opposite of what the
// budget protects.
func (b *Budget) Refund(program uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if held, ok := b.buckets[program]; ok {
		held.tokens += float64(b.cost)
	}
}
