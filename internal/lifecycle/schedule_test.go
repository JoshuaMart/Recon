package lifecycle

import (
	"testing"
	"time"
)

func half() float64 { return 0.5 } // no jitter

func TestScheduleActive(t *testing.T) {
	c := DefaultCadences
	keep := t0.Add(48 * time.Hour)
	p := c.Schedule(Input{State: Active, Previous: Active, Stage: "resolve", NextFull: &keep}, t0, half)
	if !p.NextResolve.Equal(t0.Add(24*time.Hour)) || !p.NextFull.Equal(keep) {
		t.Errorf("after a resolve the full date is kept: %v %v", p.NextResolve, p.NextFull)
	}
	p = c.Schedule(Input{State: Active, Previous: Active, Stage: "full", NextFull: &keep}, t0, half)
	if !p.NextFull.Equal(t0.Add(72 * time.Hour)) {
		t.Errorf("after a full: %v", p.NextFull)
	}
}

func TestSchedulePromotion(t *testing.T) {
	p := DefaultCadences.Schedule(Input{State: Active, Previous: Candidate, Stage: "resolve"}, t0, func() float64 { return 0.99 })
	if p.NextFull == nil || p.NextFull.After(t0.Add(5*time.Minute)) {
		t.Errorf("a promoted candidate gets a full within 5 min: %v", p.NextFull)
	}
}

func TestScheduleCandidateCurveThenGivesUp(t *testing.T) {
	c := DefaultCadences
	first := t0
	now := t0
	tier := 0
	for i, want := range c.Candidate {
		p := c.Schedule(Input{State: Candidate, Tier: tier, FirstSeen: first}, now, half)
		if p.Archive || !p.NextResolve.Equal(now.Add(want)) {
			t.Fatalf("rung %d = %+v, want +%s", i, p, want)
		}
		now, tier = *p.NextResolve, p.Tier
	}
	// The curve sums to about four days: before the give-up age it waits on the last rung.
	if p := c.Schedule(Input{State: Candidate, Tier: tier, FirstSeen: first}, now, half); p.Archive {
		t.Fatal("archived before the give-up age")
	}
	if p := c.Schedule(Input{State: Candidate, Tier: tier, FirstSeen: first}, t0.Add(15*24*time.Hour), half); !p.Archive {
		t.Fatal("not archived after the give-up age")
	}
}

func TestScheduleFlappingUsesTheCheapestStage(t *testing.T) {
	c := DefaultCadences
	p := c.Schedule(Input{State: Flapping, Stage: "resolve", Failing: map[string]bool{"dns": true}}, t0, half)
	if !p.NextResolve.Equal(t0.Add(15*time.Minute)) || !p.NextFull.Equal(t0.Add(72*time.Hour)) {
		t.Errorf("dns failing: %+v", p)
	}
	p = c.Schedule(Input{State: Flapping, Failing: map[string]bool{"http": true}}, t0, half)
	if !p.NextFull.Equal(t0.Add(time.Hour)) {
		t.Errorf("http failing: full must respect the 1 h floor, got %v", p.NextFull)
	}
	p = c.Schedule(Input{State: Flapping, Tier: 99, Failing: map[string]bool{"dns": true}}, t0, half)
	if !p.NextResolve.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("past the curve it stays on its last rung: %v", p.NextResolve)
	}
}

func TestJitterSpreads(t *testing.T) {
	c := DefaultCadences
	lo := c.Schedule(Input{State: Active, Stage: "full"}, t0, func() float64 { return 0 })
	hi := c.Schedule(Input{State: Active, Stage: "full"}, t0, func() float64 { return 0.999 })
	if !lo.NextFull.Before(*hi.NextFull) || lo.NextFull.Before(t0.Add(64*time.Hour)) || hi.NextFull.After(t0.Add(80*time.Hour)) {
		t.Errorf("jitter range: %v .. %v", lo.NextFull, hi.NextFull)
	}
}

// Whatever the state, the stage that just ran never stays due: a date at or
// before now would make the host due again at once, in a loop. A resolve only
// runs when no full is due, so its input has the full date ahead.
func TestNoDateStaysInThePast(t *testing.T) {
	c := DefaultCadences
	past, ahead := t0.Add(-time.Hour), t0.Add(time.Hour)
	for _, state := range []string{Active, Flapping, Inactive, Unobservable, Candidate} {
		for _, stage := range []string{"resolve", "full"} {
			for _, failing := range []map[string]bool{{}, {"dns": true}, {"tcp": true}, {"http": true}} {
				full := &past
				if stage == "resolve" {
					full = &ahead
				}
				p := c.Schedule(Input{
					State: state, Previous: state, Stage: stage, Failing: failing,
					FirstSeen: t0, NextResolve: &past, NextFull: full,
				}, t0, half)
				for name, d := range map[string]*time.Time{"resolve": p.NextResolve, "full": p.NextFull} {
					if d != nil && !d.After(t0) {
						t.Errorf("%s after %s, failing %v: next %s at %v, not after now", state, stage, failing, name, d)
					}
				}
			}
		}
	}
}

func TestPromotionByAFullNeedsNoSecondFull(t *testing.T) {
	p := DefaultCadences.Schedule(Input{State: Active, Previous: Candidate, Stage: "full"}, t0, half)
	if !p.NextFull.Equal(t0.Add(72 * time.Hour)) {
		t.Errorf("a full that promoted already found the ports: next full %v", p.NextFull)
	}
}
