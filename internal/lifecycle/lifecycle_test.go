package lifecycle_test

import (
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/lifecycle"
)

var start = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// run folds a series of outcomes spaced by one delay each.
func run(gap time.Duration, outcomes ...string) lifecycle.Counters {
	var counters lifecycle.Counters
	at := start
	for _, outcome := range outcomes {
		counters = lifecycle.Next(counters, outcome, at)
		at = at.Add(gap)
	}
	return counters
}

func TestThreeInformativeFailuresOverADayAreADeath(t *testing.T) {
	counters := run(12*time.Hour, lifecycle.OutcomeFail, lifecycle.OutcomeFail, lifecycle.OutcomeFail)

	if counters.State != lifecycle.LayerDead {
		t.Fatalf("three nxdomains spread over 24 h left the layer %q", counters.State)
	}
	if state := lifecycle.Decide(lifecycle.Active, counters); state != lifecycle.Inactive {
		t.Fatalf("a dead layer left the asset %q", state)
	}
}

// The floor is the assertion, not the count. Without it the threshold is not a
// threshold: the backoff curve can deliver three failures inside ninety
// minutes, which is not long enough to tell an outage from a disappearance.
func TestThreeInformativeFailuresInNinetyMinutesAreNot(t *testing.T) {
	counters := run(45*time.Minute, lifecycle.OutcomeFail, lifecycle.OutcomeFail, lifecycle.OutcomeFail)

	if counters.State != lifecycle.LayerFailing {
		t.Fatalf("three nxdomains inside 90 min left the layer %q, and the 24 h floor is what separates them", counters.State)
	}
	if state := lifecycle.Decide(lifecycle.Active, counters); state != lifecycle.Flapping {
		t.Fatalf("the asset went to %q on failures that span less than a day", state)
	}
}

// The one signal that must never conclude anything. A timeout is
// indistinguishable from a filter or a ban, and an inventory that reads it as a
// death archives every host behind a firewall that started dropping.
func TestARepeatedTimeoutNeverProducesADeath(t *testing.T) {
	counters := run(24*time.Hour,
		lifecycle.OutcomeError, lifecycle.OutcomeError, lifecycle.OutcomeError,
		lifecycle.OutcomeError, lifecycle.OutcomeError, lifecycle.OutcomeError)

	if counters.State == lifecycle.LayerDead {
		t.Fatal("six timeouts spread over six days produced a death")
	}
	if counters.NonInformative != 6 {
		t.Fatalf("the non informative counter is %d, and it is what phase 3 reads", counters.NonInformative)
	}
	if state := lifecycle.Decide(lifecycle.Active, counters); state != lifecycle.Active {
		t.Fatalf("timeouts moved the asset to %q, when nothing was measured at all", state)
	}
}

func TestOneSuccessIsTheWholeOfTheRecoveryRule(t *testing.T) {
	failing := run(12*time.Hour, lifecycle.OutcomeFail, lifecycle.OutcomeFail)
	if state := lifecycle.Decide(lifecycle.Active, failing); state != lifecycle.Flapping {
		t.Fatalf("two failures left the asset %q", state)
	}

	recovered := lifecycle.Next(failing, lifecycle.OutcomeOK, start.Add(36*time.Hour))
	if recovered.Informative != 0 || !recovered.FirstFailureAt.IsZero() {
		t.Fatalf("a success left %d failures and a first failure at %s",
			recovered.Informative, recovered.FirstFailureAt)
	}
	if state := lifecycle.Decide(lifecycle.Flapping, recovered); state != lifecycle.Active {
		t.Fatalf("a single success left the asset %q", state)
	}
}

// A timeout between two nxdomains is not proof the name came back, so it must
// not reset the run. Only a success does, which is the reading that makes
// "consecutive" mean what the threshold needs it to mean.
func TestATimeoutDoesNotEraseAFailureRun(t *testing.T) {
	counters := run(12*time.Hour,
		lifecycle.OutcomeFail, lifecycle.OutcomeError, lifecycle.OutcomeFail, lifecycle.OutcomeFail)

	if counters.Informative != 3 {
		t.Fatalf("the informative run is %d, and an inconclusive probe in the middle is what it survived", counters.Informative)
	}
	if counters.State != lifecycle.LayerDead {
		t.Fatalf("the layer is %q after three qualified failures spread over 36 h", counters.State)
	}
}

// The case that forces the most-severe rule. On a fronted asset whose origin is
// dead, dns resolves and tcp connects because the edge answers for both. Under
// any rule where a success somewhere restores the asset, the only death signal
// available behind a CDN is thrown away.
func TestADeadOriginBehindALiveEdgeStillDies(t *testing.T) {
	edge := run(24*time.Hour, lifecycle.OutcomeOK)
	origin := run(12*time.Hour, lifecycle.OutcomeFail, lifecycle.OutcomeFail, lifecycle.OutcomeFail)

	if state := lifecycle.Decide(lifecycle.Active, edge, edge, origin); state != lifecycle.Inactive {
		t.Fatalf("dns ok, tcp ok and a dead http layer gave %q", state)
	}
}

func TestALayerNothingMeasuredHasNoOpinion(t *testing.T) {
	healthy := run(0, lifecycle.OutcomeOK)

	if state := lifecycle.Decide(lifecycle.Candidate, healthy, lifecycle.Counters{}); state != lifecycle.Active {
		t.Fatalf("an unmeasured layer beside a healthy one gave %q", state)
	}
	if state := lifecycle.Decide(lifecycle.Candidate, lifecycle.Counters{}); state != lifecycle.Candidate {
		t.Fatalf("an asset nothing has probed became %q", state)
	}
}

// Archived is out of the scheduler and comes back by hand or on rediscovery,
// never because a stray observation landed on it.
func TestArchivedIsNotReopenedByAnObservation(t *testing.T) {
	healthy := run(0, lifecycle.OutcomeOK)
	if state := lifecycle.Decide(lifecycle.Archived, healthy); state != lifecycle.Archived {
		t.Fatalf("an archived asset was reopened as %q", state)
	}
}

func TestACurveClampsRatherThanWraps(t *testing.T) {
	last := lifecycle.CandidateCurve[len(lifecycle.CandidateCurve)-1]
	if got := lifecycle.CandidateCurve.At(99); got != last {
		t.Fatalf("tier 99 gave %s, and wrapping to the first rung would probe a hopeless candidate every minute forever", got)
	}
	if got := lifecycle.CandidateCurve.At(-1); got != lifecycle.CandidateCurve[0] {
		t.Fatalf("tier -1 gave %s", got)
	}
}

// Proportional rather than additive: the two curves span four orders of
// magnitude, and fifteen minutes of jitter on a one minute rung deletes the
// rung.
func TestJitterNeverSwallowsAShortRung(t *testing.T) {
	cadence := lifecycle.DefaultCadence()

	short := cadence.Spread(time.Minute, 0.999)
	if short > 75*time.Second {
		t.Fatalf("a one minute rung became %s", short)
	}
	long := cadence.Spread(24*time.Hour, 1)
	if long < 24*time.Hour || long > 24*time.Hour+cadence.Jitter {
		t.Fatalf("a day became %s, outside [24h, 24h15m]", long)
	}
	if exact := cadence.Spread(24*time.Hour, 0); exact != 24*time.Hour {
		t.Fatalf("no jitter gave %s", exact)
	}
}

func TestTheTierResetsWhenAnAssetComesBack(t *testing.T) {
	if tier := lifecycle.NextTier(lifecycle.Flapping, 2); tier != 3 {
		t.Fatalf("a failing asset went to tier %d", tier)
	}
	if tier := lifecycle.NextTier(lifecycle.Active, 6); tier != 0 {
		t.Fatalf("an asset that came back kept tier %d, so it would be probed on the widened delay it earned while it was down", tier)
	}
}

func TestACandidateBudgetRunsOut(t *testing.T) {
	if lifecycle.Exhausted(start, start.Add(13*24*time.Hour)) {
		t.Fatal("a candidate was given up on at 13 days")
	}
	if !lifecycle.Exhausted(start, start.Add(15*24*time.Hour)) {
		t.Fatal("a candidate was still being chased at 15 days")
	}
}
