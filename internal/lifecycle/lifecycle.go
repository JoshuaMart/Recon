// Package lifecycle holds the state machine and the backoff curves, as pure
// functions: ingestion calls them in the transaction that writes the
// observation, and tests call them alone.
package lifecycle

import "time"

// States.
const (
	Candidate    = "candidate"
	Active       = "active"
	Flapping     = "flapping"
	Inactive     = "inactive"
	Unobservable = "unobservable"
	Archived     = "archived"
)

// Outcomes, as on observations.
const (
	OK    = "ok"
	Fail  = "fail"
	Error = "error"
)

// Thresholds.
const (
	// DeadFailures is the run of informative failures that, with DeadFloor
	// elapsed since the first, makes a layer dead.
	DeadFailures = 3
	DeadFloor    = 24 * time.Hour
	// UnobservableErrors is the run of non informative failures, on every
	// observer, that makes an asset unobservable.
	UnobservableErrors = 3
)

// Layer is one layer's counters, as in asset_layer.
type Layer struct {
	Informative    int
	NonInformative int
	FirstFailure   *time.Time
	LastOK         *time.Time
}

// Apply updates a layer with one outcome. countFailures is false for a recon:
// an enumeration never counts toward a death, it can only reset.
func Apply(l Layer, outcome string, now time.Time, countFailures bool) Layer {
	switch outcome {
	case OK:
		return Layer{LastOK: &now}
	case Fail:
		if !countFailures {
			return l
		}
		if l.Informative == 0 {
			l.FirstFailure = &now
		}
		l.Informative++
		l.NonInformative = 0
	case Error:
		if !countFailures {
			return l
		}
		l.NonInformative++
	}
	return l
}

// reading is what one layer says.
type reading int

const (
	healthy reading = iota
	failing
	dead
)

func read(l Layer, now time.Time) reading {
	switch {
	case l.Informative >= DeadFailures && l.FirstFailure != nil && now.Sub(*l.FirstFailure) >= DeadFloor:
		return dead
	case l.Informative > 0:
		return failing
	}
	return healthy
}

// Observers are the layers that can get a result on each kind: an asset is
// unobservable only when every one of them tried and got nothing.
var Observers = map[string][]string{
	"fqdn":    {"dns"},
	"service": {"http", "fingerprint"},
	"url":     {"http", "fingerprint"},
}

// Verdict is the asset's state from its layers. okNow tells whether a layer
// answered ok in this pass, which is what revives a candidate or an archived
// asset. The worst layer with an opinion wins, and the fingerprint layer never
// moves an asset toward a death.
func Verdict(kind, current string, layers map[string]Layer, okNow bool, now time.Time) string {
	switch current {
	case Candidate, Archived:
		if !okNow {
			return current
		}
	}
	worst := healthy
	for name, l := range layers {
		if name == "fingerprint" {
			continue
		}
		if r := read(l, now); r > worst {
			worst = r
		}
	}
	switch worst {
	case dead:
		return Inactive
	case failing:
		return Flapping
	}
	if unobservable(kind, layers) {
		return Unobservable
	}
	return Active
}

func unobservable(kind string, layers map[string]Layer) bool {
	observers := Observers[kind]
	if len(observers) == 0 {
		return false
	}
	for _, o := range observers {
		// An observer that never ran is not one that fails.
		l, ok := layers[o]
		if !ok || l.NonInformative < UnobservableErrors {
			return false
		}
	}
	return true
}
