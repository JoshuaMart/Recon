// Package lifecycle decides what an observation says about an asset.
//
// Everything here is a pure function over the previous counters and the
// observation that just arrived. It is called from the ingestion transaction,
// which is where the transitions have to live: an asynchronous component can
// fail between the write and the transition and leave an asset in a state that
// contradicts its last observation, and it would have to re-parse the payload
// ingestion has already read to tell an nxdomain from a timeout.
//
// Splitting it out of that transaction anyway is what makes it testable
// without a database, which is most of why the thresholds below can be
// asserted at all.
package lifecycle

import "time"

// Layer verdicts. A layer either has never been measured, is holding, is
// failing below the threshold, or has reached it.
const (
	LayerUnmeasured = "unmeasured"
	LayerHealthy    = "healthy"
	LayerFailing    = "failing"
	LayerDead       = "dead"
)

// Asset states. unobservable is deliberately absent from what this package
// decides: it needs a second observer, and the fingerprinter is phase 3.
const (
	Candidate    = "candidate"
	Active       = "active"
	Flapping     = "flapping"
	Inactive     = "inactive"
	Unobservable = "unobservable"
	Archived     = "archived"
)

// Outcomes, which are not "succeeded, failed, crashed" but what was learned
// about the target: it answered and it is there, it answered and it is not, or
// nothing conclusive was obtained.
const (
	OutcomeOK    = "ok"
	OutcomeFail  = "fail"
	OutcomeError = "error"
)

// InformativeThreshold is how many qualified failures a death takes.
//
// Three is enough because they are qualified: an nxdomain confirmed three
// times with resolver consensus leaves no ambiguity. A larger count would be
// compensating for the absence of qualification with volume.
const InformativeThreshold = 3

// DeathFloor is the window those failures have to span.
//
// Without it the threshold is not a threshold: the backoff curve can deliver
// three failures inside ninety minutes, which is not long enough to tell an
// outage from a disappearance.
const DeathFloor = 24 * time.Hour

// Counters are one layer's memory. The zero value is a layer nothing has ever
// measured, which is not the same as one that is holding.
type Counters struct {
	State string
	// Informative counts consecutive failures where an observer reached the
	// target and reported an absence. Only a success resets it: a timeout
	// between two nxdomains is not proof the name came back.
	Informative int
	// NonInformative counts consecutive failures where nothing conclusive was
	// obtained. It feeds unobservable in phase 3 and never a death.
	NonInformative int
	// FirstFailureAt is when the current informative run began. It is what
	// makes the 24 hour floor checkable at all.
	FirstFailureAt time.Time
	LastOKAt       time.Time
	LastCheckedAt  time.Time
}

// Measured reports whether this layer has an opinion. A layer nothing ever
// probed is ignored when the asset's state is decided, rather than counted as
// healthy.
func (c Counters) Measured() bool { return c.State != "" && c.State != LayerUnmeasured }

// Next folds one observation into a layer's counters.
//
// The outcome has already been qualified at ingestion, from the payload and
// from what the run said about itself. This function trusts that qualification
// and nothing else: it never looks at a payload, which is what keeps the
// thresholds readable.
func Next(prev Counters, outcome string, at time.Time) Counters {
	next := prev
	next.LastCheckedAt = at

	switch outcome {
	case OutcomeOK:
		// A single success is the whole of the recovery rule. It resets both
		// counters, because a target that answered was reached, which settles
		// the observer's question as well as the target's.
		next.Informative = 0
		next.NonInformative = 0
		next.FirstFailureAt = time.Time{}
		next.LastOKAt = at
		next.State = LayerHealthy

	case OutcomeFail:
		next.Informative++
		// A failure the target itself reported means an observer got through,
		// so the consecutive run of inconclusive probes is over.
		next.NonInformative = 0
		if next.FirstFailureAt.IsZero() {
			next.FirstFailureAt = at
		}
		next.State = LayerFailing
		if next.Informative >= InformativeThreshold &&
			!at.Before(next.FirstFailureAt.Add(DeathFloor)) {
			next.State = LayerDead
		}

	default:
		// Nothing conclusive. The state is left where it was on purpose: a
		// timeout must not make an asset look unstable, because flapping is a
		// buffer on the way to a death and this signal can never justify one.
		// It makes the asset unmeasurable, which is a different column.
		next.NonInformative++
		if next.State == "" {
			next.State = LayerUnmeasured
		}
	}

	return next
}

// Revived reports whether an archived asset comes back on this observation.
//
// It reads the observation and never the layer's state, and that distinction is
// the whole rule. A layer keeps the state of its last conclusive measurement,
// so an asset archived months after a success still carries a healthy dns
// layer: reading the column would let a timeout arriving today revive it, which
// is the opposite of what happened.
//
// An archived asset carries no due date, so the only observation that can reach
// one is an enumeration finding it again. That is what rediscovery means here,
// and it is one of the two documented ways back. The other is somebody entering
// the asset by hand, which is an act rather than an observation and does not go
// through here.
func Revived(current, outcome string) bool {
	return current == Archived && outcome == OutcomeOK
}

// Decide reads the asset's state off its layers.
//
// The most severe layer wins, and that rule is what the CDN case forces. On a
// fronted asset whose origin is dead, dns resolves and tcp connects because the
// edge answers for both, while http returns a recognized origin error. Under
// any rule where a success somewhere restores the asset, it stays active
// forever and the only death signal available behind a CDN is thrown away.
//
// The counterpart holds: a success on the failing layer resets its counters, so
// the asset returns to active in a single probe.
func Decide(current string, layers ...Counters) string {
	worst := ""
	for _, layer := range layers {
		if !layer.Measured() {
			continue
		}
		switch layer.State {
		case LayerDead:
			worst = LayerDead
		case LayerFailing:
			if worst != LayerDead {
				worst = LayerFailing
			}
		case LayerHealthy:
			if worst == "" {
				worst = LayerHealthy
			}
		}
	}

	// Archived is out of the scheduler and comes back through Revived, which
	// reads the observation rather than these counters.
	if current == Archived {
		return Archived
	}

	switch worst {
	case LayerDead:
		return Inactive
	case LayerFailing:
		return Flapping
	case LayerHealthy:
		return Active
	default:
		// Nothing has an opinion. An asset derived from an open port and never
		// probed sits here, which is exactly what candidate means.
		if current == "" {
			return Candidate
		}
		return current
	}
}
