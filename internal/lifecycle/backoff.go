package lifecycle

import "time"

// Curve is a backoff ladder. There are two because one curve tuned between the
// two questions would answer neither.
type Curve []time.Duration

// CandidateCurve is aggressive at the start. Between certificate issuance and
// the service going live, minutes to days pass; probing often in the first hour
// catches it as it appears.
var CandidateCurve = Curve{
	time.Minute, 5 * time.Minute, 15 * time.Minute,
	time.Hour, 6 * time.Hour, 24 * time.Hour, 72 * time.Hour,
}

// FlappingCurve is patient: a false positive is a useless alert, while waiting
// costs a few hours of staleness.
var FlappingCurve = Curve{
	15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour,
}

// CandidateBudget is how long a candidate is chased before being given up on.
// It ends archived, not inactive: infrastructure never provisioned is not dead.
const CandidateBudget = 14 * 24 * time.Hour

// At reads the delay for a tier, clamped at both ends. A tier past the end
// stays on the last rung instead of wrapping to the first.
func (c Curve) At(tier int) time.Duration {
	if len(c) == 0 {
		return 0
	}
	if tier < 0 {
		tier = 0
	}
	if tier >= len(c) {
		tier = len(c) - 1
	}
	return c[tier]
}

// Exhausted reports whether a candidate has been chased long enough.
func Exhausted(firstSeen, at time.Time) bool {
	return !firstSeen.IsZero() && !at.Before(firstSeen.Add(CandidateBudget))
}

// Render priorities, sorted ascending before the due date (lower is served
// first). A baseline enters low: after a mass discovery there are thousands of
// first renders, and they must not queue ahead of a change someone is waiting on.
const (
	PriorityChange   int16 = 100
	PriorityBaseline int16 = 200
)

// Cadence is the nominal rate of each rung. It is configuration because the
// right values depend on perimeter size and programme rules.
type Cadence struct {
	// Resolve costs one round trip to the resolver pool; nothing reaches the target.
	Resolve time.Duration
	// Full is a hundred connections per host plus an HTTP probe, by far the
	// costliest rung.
	Full time.Duration
	// Fingerprint is the nominal render cadence, per service rather than per host.
	// It is not modulated by volatility: tiers need weeks of real data, and
	// thresholds fixed on a few hundred assets would later read as measurements.
	Fingerprint time.Duration
	// RenderSole is the cadence when the renderer is the only detector left.
	RenderSole time.Duration
	// RenderRecovery is the cadence when the renderer itself is turned away. It is
	// a recovery attempt, so it is rare.
	RenderRecovery time.Duration
	// RenderBlind is the cadence when neither observer gets through. The asset is
	// unmeasurable, not dead, so it stays short.
	RenderBlind time.Duration
	// Inactive is the low rate at which a confirmed death is still watched; without
	// it a return is never noticed.
	Inactive time.Duration
	// Jitter is the ceiling of the spread added to every delay, so assets written
	// by one discovery run do not share a due date forever.
	Jitter time.Duration
	// FullFloor is the fastest the expensive rung may run on a backoff.
	//
	// Curves are written for the cheap rung. Fifteen minutes is one resolver round
	// trip but, on full, a hundred connections per host four times an hour. A
	// failing asset also holds the earliest due date, so it would head every batch
	// and starve the nominal passes.
	FullFloor time.Duration
}

// DefaultCadence is what a deployment inherits when it says nothing.
func DefaultCadence() Cadence {
	return Cadence{
		Resolve:        24 * time.Hour,
		Full:           72 * time.Hour,
		Fingerprint:    21 * 24 * time.Hour,
		RenderSole:     7 * 24 * time.Hour,
		RenderRecovery: 30 * 24 * time.Hour,
		RenderBlind:    7 * 24 * time.Hour,
		Inactive:       7 * 24 * time.Hour,
		Jitter:         15 * time.Minute,
		FullFloor:      6 * time.Hour,
	}
}

// Stagger is a due date of "now", spread.
//
// Spread widens an existing delay and cannot spread zero. That is the case
// here: a candidate that answers is due for full immediately, and a certificate
// with tens of names promotes all of them at once. Full is the entry into a
// recurring cadence, so a convoy formed there would recur every cycle.
func (c Cadence) Stagger(r float64) time.Duration {
	if c.Jitter <= 0 {
		return 0
	}
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999
	}
	return time.Duration(float64(c.Jitter) * r)
}

// Spread widens a delay by a fraction of itself, capped by the configured
// ceiling. It is proportional because the curves span four orders of magnitude;
// fifteen minutes of jitter would delete a one minute rung.
//
// The randomness r in [0, 1) is a parameter so tests assert a delay, not a range.
func (c Cadence) Spread(delay time.Duration, r float64) time.Duration {
	if delay <= 0 {
		return delay
	}
	ceiling := min(c.Jitter, delay/4)
	if ceiling <= 0 {
		return delay
	}
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999
	}
	return delay + time.Duration(float64(ceiling)*r)
}

// Delay is how long until an asset in this state is due again. The tier only
// matters while failing; an active asset gets the nominal cadence, so one
// success restores its rhythm without walking the curve back down.
func (c Cadence) Delay(state string, rung string, tier int) time.Duration {
	var delay time.Duration
	switch state {
	case Flapping:
		delay = FlappingCurve.At(tier)
	case Candidate:
		delay = CandidateCurve.At(tier)
	case Inactive, Unobservable:
		delay = c.Inactive
	case Archived:
		return 0
	default:
		if rung == RungFull {
			return c.Full
		}
		return c.Resolve
	}

	// Floor on a backoff, never on the nominal schedule: a curve written for one
	// resolver round trip must not run a full pass four times an hour on an asset
	// failing at the tcp or http layer.
	if rung == RungFull && delay < c.FullFloor {
		return c.FullFloor
	}
	return delay
}

// The rungs a due date belongs to. A run's scope decides which dates its
// report moves: full runs every rung below it, so an asset due for full needs
// no resolve run, but not the reverse.
const (
	RungResolve = "resolve"
	RungFull    = "full"
)

// NextTier is the backoff tier after one observation. It resets on any
// non-failure so a returning asset does not keep the widening it earned.
func NextTier(state string, tier int) int {
	switch state {
	case Flapping, Candidate, Inactive, Unobservable:
		return tier + 1
	default:
		return 0
	}
}
