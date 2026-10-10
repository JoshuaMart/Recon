package lifecycle

import "time"

// ReachThreshold is how many concordant results a regime change takes, in both
// directions, so a transient failure is absorbed rather than flipping.
const ReachThreshold = 3

// Reach is what the two observers have managed on one asset. The counters are
// signed (positive successes, negative failures), so the threshold reads the same
// both ways with one column per observer.
type Reach struct {
	HTTP        int
	Fingerprint int
}

// Unobservable reports whether neither observer gets through. Both must have
// tried: a streak at zero is an observer that never ran, and reading it as failing
// would tip every asset the renderer has not reached into a different state.
func (r Reach) Unobservable() bool {
	return r.HTTP <= -ReachThreshold && r.Fingerprint <= -ReachThreshold
}

// Regime is which observer currently gets a result, as the projection holds it.
// Nil is undefined, not false: observers that have not agreed three times yet give
// no regime, and treating it as failure would put every freshly rendered service
// on the alert cadence.
type Regime struct {
	HTTP        *bool
	Fingerprint *bool
}

func settled(flag *bool) bool { return flag == nil || *flag }

// Render is how long until this asset is rendered again. Four regimes, the two
// middle ones making this a table: when the raw client is blocked the renderer is
// the only detector and runs at a detector's rate; when the renderer is the one
// blocked, rendering is a recovery attempt, and paying for a browser every three
// weeks to be turned away is an expensive way to learn nothing.
func (c Cadence) Render(r Regime) time.Duration {
	http, fingerprint := settled(r.HTTP), settled(r.Fingerprint)
	switch {
	case http && fingerprint:
		return c.Fingerprint
	case !http && fingerprint:
		return c.RenderSole
	case http && !fingerprint:
		return c.RenderRecovery
	default:
		return c.RenderBlind
	}
}

// Detector reports whether the HTTP layer is the one watching this asset. If not,
// a diff on that layer no longer triggers a render: the probe keeps running for
// reachability and TLS, but what it sees of a refusing target is not worth a browser.
func (r Regime) Detector() bool { return settled(r.HTTP) }
