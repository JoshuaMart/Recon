package notify

import "time"

// Window is a priority class's sliding window.
type Window struct {
	Span time.Duration
	Cap  int
}

// Windows per priority.
//
// Critical passes through none of this. A takeover is notified with at most one
// notifier tick between the event being written and being sent, and "immediate"
// without a bound is not a testable assertion.
var Windows = map[string]Window{
	High:   {Span: 15 * time.Minute, Cap: 20},
	Medium: {Span: time.Hour, Cap: 10},
	Low:    {Span: time.Hour, Cap: 10},
}

// Aggregated reports whether a priority passes through a window at all.
func Aggregated(priority string) bool {
	_, windowed := Windows[priority]
	return windowed
}

// Windowed reports whether this event is subject to a window.
//
// Two things escape them. Critical, which the table already says. And any event
// carrying no asset, because a programme event is already an aggregate: a
// summary speaks for a batch and a mass tip speaks for an inventory. Folding
// them into a second aggregate counts them twice and, worse, loses them: twenty
// new assets saturating the high window would swallow the programme that just
// went dark into their summary, where a distinct alert is exactly what is
// wanted.
//
// The rule is written as "carries no asset" rather than as a list of types, so
// a future programme event inherits it instead of having to remember.
func Windowed(priority string, hasAsset bool) bool {
	return hasAsset && Aggregated(priority)
}

// UnobservableTiers are the ratios at which a mass tip speaks again.
//
// The cooldown lifts when the ratio crosses a higher threshold. A programme
// flagged at 12 % stays quiet at 15 % and speaks again at 30 %: an incident
// that gets worse has to say so, even inside its own window.
var UnobservableTiers = []float64{0.10, 0.25, 0.50}

// UnobservableCooldown is how long a mass tip stays quiet inside its tier.
//
// One hour, because a mass tip usually signals an address ban on the scanning
// side, which is actionable within the hour. Six hours of silence would be
// silence on an ongoing incident.
const UnobservableCooldown = time.Hour

// Tier is the highest threshold a ratio has crossed, or -1 below the first.
func Tier(ratio float64) int {
	tier := -1
	for i, threshold := range UnobservableTiers {
		if ratio >= threshold {
			tier = i
		}
	}
	return tier
}

// Speaks reports whether a mass tip is worth saying again.
//
// It speaks when nothing has been said, when the cooldown has run out, or when
// the ratio has climbed into a higher tier whatever the cooldown says.
func Speaks(ratio float64, lastTier int, lastAt time.Time, now time.Time) (bool, int) {
	tier := Tier(ratio)
	if tier < 0 {
		return false, tier
	}
	if lastAt.IsZero() || tier > lastTier {
		return true, tier
	}
	return !now.Before(lastAt.Add(UnobservableCooldown)), tier
}
