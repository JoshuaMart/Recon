package lifecycle

import "time"

// Curve is a backoff ladder. Two of them exist because they answer different
// questions, and a single curve tuned between the two answers neither.
type Curve []time.Duration

// CandidateCurve is aggressive at the start.
//
// Between a certificate being issued and the service actually going live,
// anything from a few minutes to a few days passes. Probing often during the
// first hour is what catches a service as it appears, before it is hardened,
// and that is where the freshness advantage actually is.
var CandidateCurve = Curve{
	time.Minute, 5 * time.Minute, 15 * time.Minute,
	time.Hour, 6 * time.Hour, 24 * time.Hour, 72 * time.Hour,
}

// FlappingCurve is patient, because the cost of a false positive is a useless
// alert and the cost of waiting is a few hours of staleness.
var FlappingCurve = Curve{
	15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour,
}

// CandidateBudget is how long a candidate is chased before it is given up on.
//
// It ends archived rather than inactive: an asset whose infrastructure was
// never provisioned is not dead, it never existed.
const CandidateBudget = 14 * 24 * time.Hour

// At reads the delay for a tier, clamping at both ends. A tier past the end of
// a curve stays on its last rung rather than wrapping to the first, which is
// the bug this method exists to make impossible.
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

// Render priorities. The column is sorted ascending before the due date, so a
// lower number is served first.
//
// A baseline enters low. A first render of something nobody has looked at yet
// must not queue ahead of a change somebody is waiting on, and after a mass
// discovery there are thousands of the first and a handful of the second.
const (
	PriorityChange   int16 = 100
	PriorityBaseline int16 = 200
)

// Cadence is the nominal rate of each rung, which the stage ladder is the cost
// knob for. These are configuration rather than constants: the right numbers
// depend on the size of a perimeter and on what the programme allows.
type Cadence struct {
	// Resolve costs one round trip to the resolver pool, and nothing is sent
	// to the target.
	Resolve time.Duration
	// Full is a hundred connections per host plus an HTTP probe, by far the
	// most expensive rung.
	Full time.Duration
	// Fingerprint is the nominal render cadence, and it is per service rather
	// than per host. Modulating it by volatility is deliberately left out: the
	// tiers need weeks of real data, and fixing them on a few hundred assets
	// would produce invented thresholds that later read as measurements.
	Fingerprint time.Duration
	// RenderSole is the cadence when the renderer is the only detector left,
	// which is the common shape of a mitigation aimed at a raw client.
	RenderSole time.Duration
	// RenderRecovery is the cadence when the renderer is the one being turned
	// away. It is a recovery attempt rather than a measurement, so it is rare.
	RenderRecovery time.Duration
	// RenderBlind is the cadence when neither observer gets through. It stays
	// short because the asset is unmeasurable rather than dead, and something
	// has to keep asking.
	RenderBlind time.Duration
	// Inactive is the low rate a confirmed death is still watched at. An
	// inventory that stops looking at what died never notices it come back.
	Inactive time.Duration
	// Jitter is the ceiling of the spread added to every delay. Without it the
	// thousands of assets one discovery run wrote share a due date and come
	// back together forever.
	Jitter time.Duration
	// FullFloor is how often the expensive rung may run at its fastest.
	//
	// A backoff curve is a confirmation rate, and it is written for the cheap
	// rung: fifteen minutes of resolution costs one round trip to a resolver
	// pool. The same fifteen minutes on the full rung is a hundred connections
	// per host plus an HTTP probe, four times an hour, and an asset can reach
	// the failing state from the tcp or the http layer, where the target is
	// answering and the sweep costs everything it says on the tin. Worse, a
	// failing asset always holds the earliest due date, so it takes the head of
	// every batch and starves the inventory of its nominal passes.
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
// Spread widens an existing delay by a fraction of itself, so it cannot spread a
// zero one, and that is the case this exists for: a candidate that answers is
// due for the expensive rung immediately, and a certificate carrying tens of
// names promotes all of them in the same instant. full is the entry into a
// recurring cadence rather than a rung of a curve, so a convoy formed there
// comes back every cycle for good, which is exactly what the jitter on a
// discovery run's assets exists to prevent.
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

// Spread widens a delay by a fraction of itself, bounded by the configured
// ceiling. It is proportional rather than additive because the two curves span
// four orders of magnitude: fifteen minutes of jitter on a one minute rung
// would delete the rung.
//
// The randomness is a parameter in [0, 1) rather than a package level source,
// so a test asserts a delay instead of a range.
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

// Delay is how long until an asset in this state is due again.
//
// The backoff tier only means something while an asset is failing. On an
// active one the nominal cadence applies, which is what makes recovery cheap:
// a single success puts the asset back on its normal rhythm rather than
// walking the curve back down.
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

	// The curve accelerates confirmation, and the expensive rung has a floor on
	// how fast it may be asked to confirm. Without it a curve written for one
	// round trip to a resolver pool is applied to a hundred connections per
	// host, and an asset failing on the tcp or http layer, where the target is
	// answering and the sweep really costs what it says, runs a full pass four
	// times an hour.
	//
	// The nominal cadence is never affected: this is a floor on a backoff, not
	// on the schedule.
	if rung == RungFull && delay < c.FullFloor {
		return c.FullFloor
	}
	return delay
}

// The rungs of the ladder a due date belongs to. A run's scope decides which
// dates its report moves: an asset due for full does not need a resolve run,
// because full runs every rung below it, and the reverse is not true.
const (
	RungResolve = "resolve"
	RungFull    = "full"
)

// NextTier is the backoff tier after one observation.
//
// It resets on any state that is not a failure, so an asset that comes back
// does not carry the widening it earned while it was down.
func NextTier(state string, tier int) int {
	switch state {
	case Flapping, Candidate, Inactive, Unobservable:
		return tier + 1
	default:
		return 0
	}
}
