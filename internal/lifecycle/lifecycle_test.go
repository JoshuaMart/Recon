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

// Archived is out of the scheduler and comes back on rediscovery. An archived
// asset carries no due date, so the only observation that can reach one is an
// enumeration finding it again, which is exactly what rediscovery is here.
func TestArchivedComesBackOnASuccessAndOnNothingElse(t *testing.T) {
	if !lifecycle.Revived(lifecycle.Archived, lifecycle.OutcomeOK) {
		t.Fatal("a success on an archived asset is a rediscovery, and nothing else can ever reach it")
	}

	// A failure is not a rediscovery. An observation that measured nothing, or
	// one that measured a death, must not pull an asset back into a queue it
	// left.
	for _, outcome := range []string{lifecycle.OutcomeError, lifecycle.OutcomeFail} {
		if lifecycle.Revived(lifecycle.Archived, outcome) {
			t.Errorf("%q reopened an archived asset", outcome)
		}
	}

	// And the reading is on the observation, never on the counters. A layer
	// holds the state of its last conclusive measurement, so an archived asset
	// still carrying a healthy layer would be revived by a timeout.
	healthy := run(0, lifecycle.OutcomeOK)
	if state := lifecycle.Decide(lifecycle.Archived, healthy); state != lifecycle.Archived {
		t.Fatalf("a stale healthy layer reopened an archived asset as %q", state)
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

// A backoff curve is a confirmation rate, and it is written for the cheap rung.
// Applying it unchanged to the expensive one asks for a hundred connections per
// host four times an hour, on an asset that can be failing on a layer where the
// target is answering and the sweep costs everything it says.
func TestTheExpensiveRungHasAFloorAndTheCheapOneDoesNot(t *testing.T) {
	cadence := lifecycle.DefaultCadence()

	if got := cadence.Delay(lifecycle.Flapping, lifecycle.RungResolve, 0); got != 15*time.Minute {
		t.Errorf("the cheap rung waits %s, and the curve asks for 15m", got)
	}
	if got := cadence.Delay(lifecycle.Flapping, lifecycle.RungFull, 0); got != cadence.FullFloor {
		t.Errorf("the expensive rung waits %s, want the floor of %s", got, cadence.FullFloor)
	}
	// The floor is on a backoff, never on the schedule: an asset that is fine
	// keeps its nominal cadence.
	if got := cadence.Delay(lifecycle.Active, lifecycle.RungFull, 0); got != cadence.Full {
		t.Errorf("a healthy asset waits %s, want the nominal %s", got, cadence.Full)
	}
	// And a rung the curve already puts past the floor is left alone.
	if got := cadence.Delay(lifecycle.Flapping, lifecycle.RungFull, 3); got != 24*time.Hour {
		t.Errorf("the last rung became %s", got)
	}
}
