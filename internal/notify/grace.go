package notify

import "time"

// GraceAge is the latest a first run's silence may last.
//
// A grace is an alert suppression mechanism, and its termination must not
// depend on any other component. A programme whose first run does not finish in
// a week is an incident, and absorbing it in silence is a perimeter quietly
// ceasing to be scanned.
const GraceAge = 7 * 24 * time.Hour

// GraceAssets is the inventory size that ends a grace no run will ever end.
//
// A programme fed only by manual entry or by certificate transparency never has
// a discovery run, and would stay under grace forever.
const GraceAssets = 500

// Grace is what a programme's first run is allowed to keep quiet.
type Grace struct {
	// CompletedDiscovery is whether any discovery run has ever finished.
	CompletedDiscovery bool
	// AnyDiscovery is whether one exists at all, in any state. A failed first
	// run resolves itself through this: no completed run, so the grace holds.
	AnyDiscovery bool
	Assets       int
	CreatedAt    time.Time
}

// Active reports whether the grace still holds.
//
// The second condition is what makes the threshold correct rather than a
// refinement. Written as a plain AND, the threshold ends the grace in the
// middle of the run it exists for: a perimeter of five thousand assets would
// leave the grace partway through and flood with the rest. A programme in the
// middle of a first run is not a programme without discovery.
func (g Grace) Active(at time.Time) bool {
	// A grace nobody filled in is not a grace. The zero value would otherwise
	// read as active, because a programme with no runs and no assets is exactly
	// what a fresh one looks like, and a caller that forgot to read the facts
	// would suppress silently rather than notify. An alert too many is the safe
	// direction here; a silence is not.
	if g.CreatedAt.IsZero() {
		return false
	}
	if g.CompletedDiscovery {
		return false
	}
	if !at.Before(g.CreatedAt.Add(GraceAge)) {
		return false
	}
	return g.AnyDiscovery || g.Assets < GraceAssets
}

// Suppresses reports whether this event is held back.
//
// The grace holds back new_active and nothing else. A takeover candidate found
// during a first run is exactly the finding this product exists for, and
// holding it back because the programme is new would be the worst silence the
// system can produce.
//
// And the suppression covers what was entered rather than what is derived from
// it: an asset discovered by certificate transparency under a hand entered apex
// was typed in by nobody, so it notifies normally. The condition reads the
// asset's own source, never the observation's, because a typed-in asset is born
// from a probe observation and reading that would make this branch dead code in
// production while its test passes.
func (g Grace) Suppresses(kind, discoverySource string, at time.Time) bool {
	if kind != KindNewActive || !g.Active(at) {
		return false
	}
	if g.AnyDiscovery {
		return true
	}
	return discoverySource == "manual"
}
