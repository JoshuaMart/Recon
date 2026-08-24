package ct

import (
	"time"

	"github.com/google/uuid"
)

// cache is the short term deduplication of names already written.
//
// It is a cost control and never the correctness one. What makes a SAN one
// asset is UNIQUE (program_id, kind, key) in the database, and that holds with
// this cache warm, cold or deleted. Saying which of the two carries the
// guarantee is worth the paragraph, because a cache that is load bearing on
// correctness is one nobody may ever resize.
//
// What it buys was measured rather than assumed, and the measurement narrowed
// it: only a SAN that matched an apex ever reaches the database, so the
// baseline is a handful of names an hour rather than the stream. It earns its
// place on the burst instead, which is the case that exists: an ACME loop
// reissuing across a large perimeter, a wildcard heavy apex, and the replay an
// aggregator restart produces if its recovery option is ever turned on.
//
// Keyed per programme, because the same name under two programmes is two
// assets.
//
// Two generations rather than a heap or a timestamp per entry. Rotating drops a
// whole generation at once, which bounds the memory at twice the capacity and
// costs nothing per lookup. The price is that an entry lives somewhere between
// one and two TTLs, and that is the right thing to be imprecise about: the
// consequence of an early eviction is one insert that changes nothing.
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
// entry is added once the write succeeded, so a failed write is retried by the
// next certificate rather than swallowed for a TTL.
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

// cacheKey keeps the programme and the name apart with a byte no hostname can
// carry, so two keys cannot be built from different pairs.
func cacheKey(program uuid.UUID, name string) string {
	return program.String() + "\x00" + name
}

// budget is one programme's ceiling on candidate creation for one window.
//
// A perimeter can be pointed at an apex under which the stream carries thousands
// of names, and that is one wrong apex rule away rather than hypothetical.
// Without a bound, a public feed decides the size of a customer's inventory.
//
// It says what it dropped when the window rolls. A silent cap reads as "CT found
// forty names under this apex" where the truth is "CT found four thousand and
// these are the first forty".
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

// take spends one candidate of the window, or counts a refusal.
func (b *budget) take() bool {
	if b.ceiling > 0 && b.created >= b.ceiling {
		b.dropped++
		return false
	}
	b.created++
	return true
}
