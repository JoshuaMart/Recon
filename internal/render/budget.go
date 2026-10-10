// Package render turns due assets into browser renders.
//
// The queue is a predicate re-evaluated every tick and a render has no lease:
// the due date is the queue. That makes the path idempotent, with no reservation
// to expire or crash window between a refusal and a retry.
package render

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultCost is what one render is charged against a programme's rate limit. A render is
// not one request: the browser fetches the page, its same-host subresources, then the 404
// probe, robots.txt, the sitemap and the favicon. Thirty is the order of magnitude of a real
// application page; third-party subresources cost the target nothing, so it sits below the
// browser's total and is a setting. Billing it as one would make the costliest operation the
// cheapest.
const DefaultCost = 30

// Budget meters renders against each programme's published rate limit. It is in
// memory, which assumes one control plane process; a second needs a shared
// bucket, and that is when a token bucket store returns.
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

// Charge takes a render's worth of budget, or reports there is none. It charges
// before the call: charging after lets a burst reach the target before anything
// counted it, which makes a rate limit decorative.
func (b *Budget) Charge(program uuid.UUID, rps int) bool {
	taken, _ := b.Reserve(program, rps)
	return taken
}

// Reserve takes a render's worth, or says how long until there is one.
//
// The wait is the useful half. Tokens accrue while a render runs, so a programme
// at ten requests a second affords a render every three seconds with four or
// five in flight. A caller only ever seeing a refusal would render one asset per
// pass, the budget binding the pass rather than the target.
func (b *Budget) Reserve(program uuid.UUID, rps int) (bool, time.Duration) {
	if rps <= 0 {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	held, ok := b.buckets[program]
	if !ok {
		// A programme starts with one render's worth, not a second's: a burst allowance
		// in seconds lets a large perimeter's first pass blow past its limit.
		held = &bucket{tokens: float64(b.cost), last: now}
		b.buckets[program] = held
	}

	held.tokens += now.Sub(held.last).Seconds() * float64(rps)
	held.last = now
	// The ceiling is one render, so an idle programme cannot bank a burst to spend
	// at once on someone's server.
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

// Refund gives a charge back. A saturated service means nothing reached the
// target; without this a busy renderer would throttle real renders on behalf of
// ones that never happened.
func (b *Budget) Refund(program uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if held, ok := b.buckets[program]; ok {
		held.tokens += float64(b.cost)
	}
}
