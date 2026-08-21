package lifecycle

import "time"

// ReachThreshold is how many concordant results a regime change takes, in both
// directions, so that a transient failure absorbs instead of flipping.
const ReachThreshold = 3

// Reach is what the two observers have managed on one asset.
//
// The counters are signed, positive for consecutive successes and negative for
// failures, so the threshold reads the same in both directions with one column
// per observer.
type Reach struct {
	HTTP        int
	Fingerprint int
}

// Unobservable reports whether neither observer gets through.
//
// Both must have tried. A streak at zero is an observer that never ran rather
// than one that fails, and reading it as a failure would tip every asset the
// renderer has not reached yet into a state that means something else entirely.
func (r Reach) Unobservable() bool {
	return r.HTTP <= -ReachThreshold && r.Fingerprint <= -ReachThreshold
}

// Regime is which observer currently gets a result, as the projection holds it.
//
// Nil is undefined rather than false: an asset whose observers have not yet
// agreed three times running has no regime, and treating that as a failure
// would put every freshly rendered service on the alert cadence.
type Regime struct {
	HTTP        *bool
	Fingerprint *bool
}

func settled(flag *bool) bool { return flag == nil || *flag }

// Render is how long until this asset is rendered again.
//
// Four regimes, and the two middle ones are the reason this is a table rather
// than a constant. When the raw client is blocked the renderer is the only
// detector left and has to run at a detector's rate; when the renderer is the
// one being blocked it is a recovery attempt rather than a measurement, and
// paying for a browser every three weeks to be turned away again is the
// expensive way to learn nothing.
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

// Detector reports whether the HTTP layer is the one watching this asset.
//
// When it is not, a diff on that layer stops triggering a render: the probe
// keeps running for reachability and for TLS, but what it sees of a target that
// is refusing it is not a change worth a browser.
func (r Regime) Detector() bool { return settled(r.HTTP) }
