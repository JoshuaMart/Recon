package notify

import "time"

// Window is a priority class's sliding window.
type Window struct {
	Span time.Duration
	Cap  int
}

// Windows per priority. Critical passes through none: a takeover is notified at
// most one notifier tick after being written, as "immediate" needs a bound to be
// testable.
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

// Windowed reports whether this event is subject to a window. Two things escape: Critical,
// and any event carrying no asset. A programme event is already an aggregate (a summary
// speaks for a batch, a mass tip for an inventory), and folding it into another counts it
// twice and loses it: twenty new assets saturating the high window would swallow the
// programme that just went dark. Written as "carries no asset", not a list of types, so
// future programme events inherit it.
func Windowed(priority string, hasAsset bool) bool {
	return hasAsset && Aggregated(priority)
}

// UnobservableTiers are the ratios at which a mass tip speaks again. The cooldown
// lifts when the ratio crosses a higher threshold: flagged at 12 %, quiet at 15 %,
// speaking again at 30 %. A worsening incident must say so within its own window.
var UnobservableTiers = []float64{0.10, 0.25, 0.50}

// UnobservableCooldown is how long a mass tip stays quiet inside its tier. One
// hour: it usually signals an address ban on the scanning side, actionable within
// the hour, and six hours of silence would hide an ongoing incident.
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

// Speaks reports whether a mass tip is worth saying again: when nothing was said,
// when the cooldown ran out, or when the ratio climbed to a higher tier whatever
// the cooldown says.
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
