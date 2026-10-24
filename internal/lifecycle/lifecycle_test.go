package lifecycle

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// run applies a sequence of (outcome, at) on one layer of an fqdn and returns
// the states it went through.
func run(start string, steps ...struct {
	outcome string
	after   time.Duration
},
) []string {
	state, l := start, Layer{}
	var out []string
	for _, s := range steps {
		now := t0.Add(s.after)
		l = Apply(l, s.outcome, now, true)
		state = Verdict("fqdn", state, map[string]Layer{"dns": l}, s.outcome == OK, now)
		out = append(out, state)
	}
	return out
}

type step = struct {
	outcome string
	after   time.Duration
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTakenDownGoesFlappingThenInactive(t *testing.T) {
	got := run(Active,
		step{Fail, 0}, step{Fail, 15 * time.Minute}, step{Fail, 75 * time.Minute},
		step{Fail, 7 * time.Hour}, step{Fail, 25 * time.Hour})
	want := []string{Flapping, Flapping, Flapping, Flapping, Inactive}
	if !equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestThreeFailuresInAnHourAreNotADeath(t *testing.T) {
	got := run(Active, step{Fail, 0}, step{Fail, 15 * time.Minute}, step{Fail, time.Hour})
	if got[2] != Flapping {
		t.Errorf("got %v: the 24 h floor must hold", got)
	}
}

func TestOneSuccessRestores(t *testing.T) {
	got := run(Active, step{Fail, 0}, step{Fail, time.Hour}, step{OK, 2 * time.Hour})
	if got[2] != Active {
		t.Errorf("got %v", got)
	}
}

func TestTimeoutsNeverKill(t *testing.T) {
	var steps []step
	for i := range 20 {
		steps = append(steps, step{Error, time.Duration(i) * 24 * time.Hour})
	}
	got := run(Active, steps...)
	for i, s := range got {
		if s == Inactive || s == Flapping {
			t.Fatalf("step %d is %s: a timeout is not informative", i, s)
		}
	}
	if got[1] != Active || got[2] != Unobservable {
		t.Errorf("got %v, want unobservable from the third error", got[:4])
	}
	if s := run(Unobservable, step{OK, 0}); s[0] != Active {
		t.Errorf("one success leaves unobservable, got %v", s)
	}
}

func TestCandidateHasTwoExits(t *testing.T) {
	var steps []step
	for i := range 6 {
		steps = append(steps, step{Fail, time.Duration(i) * 24 * time.Hour})
	}
	for _, s := range run(Candidate, steps...) {
		if s != Candidate {
			t.Fatalf("a failing candidate became %s", s)
		}
	}
	if s := run(Candidate, step{OK, 0}); s[0] != Active {
		t.Errorf("an answering candidate = %v", s)
	}
}

func TestArchivedRevivesOnSuccessOnly(t *testing.T) {
	if s := run(Archived, step{Error, 0}, step{Fail, time.Hour}); s[1] != Archived {
		t.Errorf("got %v", s)
	}
	if s := run(Archived, step{OK, 0}); s[0] != Active {
		t.Errorf("got %v", s)
	}
}

func TestReconNeverCountsTowardDeath(t *testing.T) {
	l := Layer{}
	for range 5 {
		l = Apply(l, Fail, t0.Add(48*time.Hour), false)
	}
	if l.Informative != 0 {
		t.Errorf("a recon counted %d failures", l.Informative)
	}
}

func TestWorstLayerWins(t *testing.T) {
	// Behind a CDN: dns and tcp answer, http says the origin is gone.
	first := t0
	layers := map[string]Layer{
		"dns":  {LastOK: &t0},
		"tcp":  {LastOK: &t0},
		"http": {Informative: 3, FirstFailure: &first},
	}
	if s := Verdict("service", Active, layers, true, t0.Add(25*time.Hour)); s != Inactive {
		t.Errorf("got %s", s)
	}
	layers["http"] = Layer{}
	layers["fingerprint"] = Layer{Informative: 9, FirstFailure: &first}
	if s := Verdict("service", Active, layers, true, t0.Add(25*time.Hour)); s != Active {
		t.Errorf("the fingerprint layer moved the asset to %s", s)
	}
}

func TestServiceUnobservableNeedsBothObservers(t *testing.T) {
	layers := map[string]Layer{"http": {NonInformative: 5}}
	if s := Verdict("service", Active, layers, false, t0); s != Active {
		t.Errorf("with the renderer never run, got %s", s)
	}
	layers["fingerprint"] = Layer{NonInformative: 3}
	if s := Verdict("service", Active, layers, false, t0); s != Unobservable {
		t.Errorf("both observers failing, got %s", s)
	}
}
