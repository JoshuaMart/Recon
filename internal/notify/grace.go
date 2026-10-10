package notify

import "time"

// GraceAge is the latest a first run's silence may last. A grace suppresses
// alerts, so its end must not depend on any other component: a programme whose
// first run takes over a week is an incident, and absorbing it silently is a
// perimeter quietly ceasing to be scanned.
const GraceAge = 7 * 24 * time.Hour

// GraceAssets is the inventory size that ends a grace no run will end. A programme
// fed only by manual entry or CT never has a discovery run and would stay under
// grace forever.
const GraceAssets = 500

// Grace is what a programme's first run is allowed to keep quiet.
type Grace struct {
	// CompletedDiscovery is whether any discovery run has ever finished.
	CompletedDiscovery bool
	// AnyDiscovery is whether a run exists in any state. A failed first run resolves
	// itself through this: no completed run, so the grace holds.
	AnyDiscovery bool
	Assets       int
	CreatedAt    time.Time
}

// Active reports whether the grace still holds. The second condition makes the
// threshold correct: as a plain AND it would end the grace mid-run (a
// five-thousand-asset perimeter leaving it partway and flooding with the rest). A
// programme in the middle of a first run is not a programme without discovery.
func (g Grace) Active(at time.Time) bool {
	// A grace nobody filled in is not a grace. The zero value would read as active (a
	// fresh programme has no runs or assets) and a caller forgetting to read the facts
	// would suppress silently. An alert too many is the safe direction.
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

// Suppresses reports whether this event is held back. The grace holds back new_active only:
// a takeover candidate during a first run is the finding this product exists for. It covers
// what was entered, not what derives from it (an asset found by CT under a hand-entered
// apex was typed by nobody and notifies normally), and reads the asset's own source, never
// the observation's: a typed-in asset is born from a probe observation, which would make
// this branch dead code in production with a passing test.
func (g Grace) Suppresses(kind, discoverySource string, at time.Time) bool {
	if kind != KindNewActive || !g.Active(at) {
		return false
	}
	if g.AnyDiscovery {
		return true
	}
	return discoverySource == "manual"
}
