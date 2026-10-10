// Package lifecycle decides what an observation says about an asset.
//
// Everything here is a pure function over the previous counters and the new
// observation. It is called from the ingestion transaction because transitions
// must not lag the write, but it is a separate package so the thresholds can be
// tested without a database.
package lifecycle

import "time"

// Layer verdicts: never measured, holding, failing below the threshold, or
// past it.
const (
	LayerUnmeasured = "unmeasured"
	LayerHealthy    = "healthy"
	LayerFailing    = "failing"
	LayerDead       = "dead"
)

// Asset states. Unobservable is not decided here: it needs a second observer
// (the fingerprinter).
const (
	Candidate    = "candidate"
	Active       = "active"
	Flapping     = "flapping"
	Inactive     = "inactive"
	Unobservable = "unobservable"
	Archived     = "archived"
)

// Outcomes describe what was learned about the target: it answered and is
// there, it answered and is not, or nothing conclusive.
const (
	OutcomeOK    = "ok"
	OutcomeFail  = "fail"
	OutcomeError = "error"
)

// InformativeThreshold is how many qualified failures a death takes. Three is
// enough because each is qualified (e.g. nxdomain with resolver consensus).
const InformativeThreshold = 3

// DeathFloor is the window those failures must span. The backoff curve can
// deliver three failures within ninety minutes, too short to tell an outage
// from a disappearance.
const DeathFloor = 24 * time.Hour

// Counters are one layer's memory. The zero value is a layer never measured,
// which differs from one that is holding.
type Counters struct {
	State string
	// Informative counts consecutive failures where an observer reached the target
	// and reported an absence. Only a success resets it: a timeout between two
	// nxdomains does not mean the name came back.
	Informative int
	// NonInformative counts consecutive inconclusive failures. It feeds
	// unobservable, never a death.
	NonInformative int
	// FirstFailureAt is when the current informative run began, to check DeathFloor.
	FirstFailureAt time.Time
	LastOKAt       time.Time
	LastCheckedAt  time.Time
}

// Measured reports whether this layer has an opinion. Unprobed layers are
// ignored when deciding state, not counted as healthy.
func (c Counters) Measured() bool { return c.State != "" && c.State != LayerUnmeasured }

// Next folds one observation into a layer's counters. The outcome was
// qualified at ingestion; this never looks at a payload.
func Next(prev Counters, outcome string, at time.Time) Counters {
	next := prev
	next.LastCheckedAt = at

	switch outcome {
	case OutcomeOK:
		// One success is the whole recovery rule: the target answered, so the
		// observer's question is settled too.
		next.Informative = 0
		next.NonInformative = 0
		next.FirstFailureAt = time.Time{}
		next.LastOKAt = at
		next.State = LayerHealthy

	case OutcomeFail:
		next.Informative++
		// The target itself reported a failure, so the inconclusive run is over.
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
		// Inconclusive: the state stays put. A timeout must not make an asset look
		// unstable, since flapping buffers a death that this signal can never justify.
		next.NonInformative++
		if next.State == "" {
			next.State = LayerUnmeasured
		}
	}

	return next
}

// Revived reports whether an archived asset comes back on this observation. It reads the
// observation, never the layer's state: a layer keeps its last conclusive state, so reading
// it would let a timeout today revive an asset archived months after a success. An archived
// asset has no due date, so only an enumeration finding it again reaches here; manual entry,
// the other way back, is an act and bypasses this.
func Revived(current, outcome string) bool {
	return current == Archived && outcome == OutcomeOK
}

// Decide reads the asset's state off its layers and observers.
//
// The most severe layer wins, which the CDN case forces: with a dead origin,
// dns and tcp still succeed at the edge while http reports an origin error.
// If a success elsewhere restored the asset, the only death signal behind a CDN
// would be lost. A success on the failing layer resets it, so recovery takes
// one probe.
func Decide(current string, scheduled bool, reach Reach, layers ...Counters) string {
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

	// Archived leaves the scheduler and returns through Revived, which reads the
	// observation, not these counters.
	if current == Archived {
		return Archived
	}

	// A candidate has two exits: a success makes it active, its budget archives it. Only
	// scheduled kinds (hosts) are held here: the budget runs in the rescheduling path, which only
	// touches hosts, so holding a service would close the one exit that emits its death.
	//
	// Failing to find a name never found is expected, not news. Passing it through flapping to
	// inactive would say a name died when it never existed and take it out of the only state the
	// budget reads, so it would never be given up on. Unobservable is refused likewise: a
	// candidate is already there.
	if current == Candidate && scheduled {
		if worst == LayerHealthy {
			return Active
		}
		return Candidate
	}

	// An observed death is not a silence and wins: an edge reporting its origin
	// gone is an informative failure even if the renderer fails at the same time.
	if worst == LayerDead {
		return Inactive
	}

	// Neither observer gets a result: the asset is unmeasurable, not unstable, so
	// it skips flapping.
	if reach.Unobservable() {
		return Unobservable
	}

	switch worst {
	case LayerFailing:
		return Flapping
	case LayerHealthy:
		return Active
	default:
		// Nothing has an opinion: an asset derived from an open port and never probed
		// is a candidate.
		if current == "" {
			return Candidate
		}
		return current
	}
}
