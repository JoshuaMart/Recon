package ct

import (
	"time"

	"github.com/google/uuid"
)

// cache is the short-term deduplication of names already written, a cost control and never
// the correctness one: a SAN being one asset is UNIQUE (program_id, kind, key) in the
// database whether this cache is warm, cold or deleted.
//
// Only a SAN matching an apex reaches the database, so the baseline is a handful of names
// an hour; the cache earns its place on bursts (an ACME loop reissuing across a large
// perimeter, a wildcard-heavy apex, an aggregator restart replaying). Keyed per programme,
// as one name under two is two assets.
//
// Two generations rather than a heap: rotating drops one at once, bounds memory at twice the
// capacity and costs nothing per lookup. An entry lives one to two TTLs, and an early
// eviction costs one insert that changes nothing.
type cache struct {
	ttl      time.Duration
	capacity int
	now      func() time.Time

	current  map[string]struct{}
	previous map[string]struct{}
	rotated  time.Time
}

func newCache(ttl time.Duration, capacity int) *cache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if capacity <= 0 {
		capacity = 50_000
	}
	return &cache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		current:  make(map[string]struct{}),
		previous: make(map[string]struct{}),
	}
}

// seen reports whether this name was written recently. It records nothing: the
// entry is added after the write succeeds, so a failed write is retried by the
// next certificate, not swallowed for a TTL.
func (c *cache) seen(program uuid.UUID, name string, at time.Time) bool {
	c.rotateIfDue(at)

	key := cacheKey(program, name)
	if _, ok := c.current[key]; ok {
		return true
	}
	_, ok := c.previous[key]
	return ok
}

// record marks a name as written.
func (c *cache) record(program uuid.UUID, name string, at time.Time) {
	c.rotateIfDue(at)
	c.current[cacheKey(program, name)] = struct{}{}
}

func (c *cache) rotateIfDue(at time.Time) {
	if c.rotated.IsZero() {
		c.rotated = at
		return
	}
	if at.Sub(c.rotated) < c.ttl && len(c.current) < c.capacity {
		return
	}
	c.previous = c.current
	c.current = make(map[string]struct{})
	c.rotated = at
}

// cacheKey separates programme and name with a byte no hostname can carry.
func cacheKey(program uuid.UUID, name string) string {
	return program.String() + "\x00" + name
}

// budget is one programme's ceiling on candidate creation per window. An apex
// under which the stream carries thousands of names is one wrong rule away, and
// without a bound a public feed decides the size of a customer's inventory.
//
// It says what it dropped when the window rolls: a silent cap reads "CT found
// forty names" when the truth is "four thousand, and these are the first forty".
type budget struct {
	ceiling int
	window  time.Duration
	started time.Time
	created int
	dropped int
}

func (m *Matcher) budgetFor(program uuid.UUID, at time.Time) *budget {
	b := m.budgets[program]
	if b == nil {
		b = &budget{ceiling: m.opts.Ceiling, window: m.opts.Window, started: at}
		m.budgets[program] = b
		return b
	}
	if at.Sub(b.started) >= b.window {
		if b.dropped > 0 {
			m.log.Warn("the certificate transparency ceiling held names back",
				"program", program, "created", b.created, "dropped", b.dropped,
				"ceiling", b.ceiling, "window", b.window)
		}
		b.started, b.created, b.dropped = at, 0, 0
	}
	return b
}

// take spends one candidate of the window, or counts a refusal. The second return
// flags the window's first refusal: the roll knows the total but an hour is long to
// stay quiet, and a programme hitting its ceiling at minute two deserves a look now.
func (b *budget) take() (bool, bool) {
	if b.ceiling > 0 && b.created >= b.ceiling {
		b.dropped++
		return false, b.dropped == 1
	}
	b.created++
	return true, false
}
