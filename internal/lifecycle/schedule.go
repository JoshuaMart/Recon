package lifecycle

import "time"

// Cadences are the configured delays. They are configuration, not constants.
type Cadences struct {
	Resolve   time.Duration   // nominal resolve cadence
	Full      time.Duration   // nominal full cadence
	Dormant   time.Duration   // inactive and unobservable assets, on full
	Promote   time.Duration   // window for the first full of a candidate that answers
	FullFloor time.Duration   // a failing full never comes back sooner
	Candidate []time.Duration // candidate curve, on resolve
	GiveUp    time.Duration   // a candidate older than this at the end of its curve is archived
	Flapping  []time.Duration // flapping curve
	Jitter    float64         // ± share applied to every delay
}

// DefaultCadences are the documented defaults.
var DefaultCadences = Cadences{
	Resolve: 24 * time.Hour, Full: 72 * time.Hour, Dormant: 7 * 24 * time.Hour,
	Promote: 5 * time.Minute, FullFloor: time.Hour,
	Candidate: []time.Duration{
		time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 72 * time.Hour,
	},
	GiveUp:   14 * 24 * time.Hour,
	Flapping: []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour},
	Jitter:   0.1,
}

// Plan is where an asset's due dates go next.
type Plan struct {
	NextResolve *time.Time
	NextFull    *time.Time
	Tier        int
	// Archive is set when a candidate ran out of its curve.
	Archive bool
}

// Input is what scheduling reads after a pass.
type Input struct {
	State     string // after the verdict
	Previous  string // before it
	Stage     string // resolve or full: what just ran
	Tier      int
	Failing   map[string]bool // layers with informative failures
	FirstSeen time.Time
	// NextResolve and NextFull are the current dates, kept when the pass did
	// not concern them.
	NextResolve *time.Time
	NextFull    *time.Time
}

// Schedule computes the next due dates. rnd returns a value in [0, 1).
func (c Cadences) Schedule(in Input, now time.Time, rnd func() float64) Plan {
	at := func(d time.Duration) *time.Time {
		j := 1 + c.Jitter*(2*rnd()-1)
		t := now.Add(time.Duration(float64(d) * j))
		return &t
	}
	switch in.State {
	case Archived:
		return Plan{}
	case Candidate:
		if in.Tier >= len(c.Candidate) {
			if now.Sub(in.FirstSeen) >= c.GiveUp {
				return Plan{Archive: true}
			}
			return Plan{NextResolve: at(c.Candidate[len(c.Candidate)-1]), Tier: in.Tier}
		}
		return Plan{NextResolve: at(c.Candidate[in.Tier]), Tier: in.Tier + 1}
	case Inactive, Unobservable:
		return Plan{NextFull: at(c.Dormant)}
	case Flapping:
		tier := min(in.Tier, len(c.Flapping)-1)
		d := c.Flapping[tier]
		p := Plan{NextResolve: in.NextResolve, NextFull: in.NextFull, Tier: in.Tier + 1}
		// The stage that just ran is measured: its date moves to the nominal
		// cadence, and a failing layer pulls it back in below. Keeping it would
		// leave it in the past, and the host would be due again at once.
		p.NextResolve = at(c.Resolve)
		if in.Stage == "full" || p.NextFull == nil {
			p.NextFull = at(c.Full)
		}
		// Each failure is re-measured by the cheapest stage that can see it.
		if in.Failing["dns"] {
			p.NextResolve = at(d)
		}
		if in.Failing["tcp"] || in.Failing["http"] {
			p.NextFull = at(max(d, c.FullFloor))
		}
		return p
	}

	// Active.
	if (in.Previous == Candidate || in.Previous == Archived) && in.Stage != "full" {
		// What made it answer is a resolve: the full that finds its ports
		// comes next, inside a window so a certificate's fifty names do not
		// form a convoy. A full that promoted it already found them.
		f := now.Add(time.Duration(rnd() * float64(c.Promote)))
		return Plan{NextResolve: at(c.Resolve), NextFull: &f}
	}
	p := Plan{NextResolve: at(c.Resolve), NextFull: in.NextFull}
	if in.Stage == "full" || p.NextFull == nil {
		p.NextFull = at(c.Full)
	}
	return p
}

// Retry moves the date of a stage that ran without reaching the host, keeping
// the other: a resolve is retried on the flapping curve's first rung, a full
// no sooner than its floor.
func (c Cadences) Retry(stage string, resolve, full *time.Time, now time.Time, rnd func() float64) (*time.Time, *time.Time) {
	at := func(d time.Duration) *time.Time {
		t := now.Add(time.Duration(float64(d) * (1 + c.Jitter*(2*rnd()-1))))
		return &t
	}
	if stage == "full" {
		full = at(c.FullFloor)
		if resolve != nil && !resolve.After(now) {
			resolve = at(c.Flapping[0])
		}
		return resolve, full
	}
	return at(c.Flapping[0]), full
}
