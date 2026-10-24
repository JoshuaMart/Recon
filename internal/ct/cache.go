package ct

import "time"

// cacheTTL is how long a name is remembered. Past it, a name seen again is
// looked at again: archived since, a new certificate revives it.
const cacheTTL = time.Hour

// cacheCap bounds one generation. It only spares the database a write:
// correctness rests on the asset's unique key.
const cacheCap = 100_000

// cache remembers keys for one to two TTLs, in two generations rather than
// per-entry expiry: rotating drops the older one at once, bounds memory at
// twice the cap and costs nothing per lookup. An early eviction costs one
// write that changes nothing.
type cache struct {
	cur, prev map[string]struct{}
	rotated   time.Time
}

// rotate starts a new generation when the current one is a TTL old or full.
func (c *cache) rotate(now time.Time) {
	if c.cur == nil {
		c.cur, c.rotated = map[string]struct{}{}, now
		return
	}
	switch age := now.Sub(c.rotated); {
	case age >= 2*cacheTTL:
		// Quiet for two TTLs: both generations are past their time.
		c.prev, c.cur, c.rotated = nil, map[string]struct{}{}, now
	case age >= cacheTTL || len(c.cur) >= cacheCap:
		c.prev, c.cur, c.rotated = c.cur, map[string]struct{}{}, now
	}
}

// seen reports whether key was recorded recently. It records nothing: a key
// is recorded once what it stands for is done, so a failure is retried.
func (c *cache) seen(key string, now time.Time) bool {
	c.rotate(now)
	if _, ok := c.cur[key]; ok {
		return true
	}
	_, ok := c.prev[key]
	return ok
}

// record remembers key from now.
func (c *cache) record(key string, now time.Time) {
	c.rotate(now)
	c.cur[key] = struct{}{}
}

// first records key and says whether it was not seen recently: a counter
// counts each name once per TTL.
func (c *cache) first(key string, now time.Time) bool {
	if c.seen(key, now) {
		return false
	}
	c.record(key, now)
	return true
}
