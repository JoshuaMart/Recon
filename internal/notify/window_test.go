package notify_test

import (
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/notify"
)

// Two things escape the windows, and the second is the one that costs an
// incident: a programme event is already an aggregate, and folding it into a
// second one loses it.
func TestAProgrammeEventIsNeverAggregated(t *testing.T) {
	t.Parallel()

	if notify.Windowed(notify.Critical, true) {
		t.Error("a takeover was put in a window")
	}
	if notify.Windowed(notify.High, false) {
		t.Error("a programme going dark was folded into the summary of twenty new assets")
	}
	if !notify.Windowed(notify.High, true) {
		t.Error("a per asset event escaped its window")
	}
}

// An incident that gets worse has to say so, even inside its own window.
func TestAMassTipSpeaksAgainWhenItGetsWorse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	speaks, tier := notify.Speaks(0.12, -1, time.Time{}, now)
	if !speaks || tier != 0 {
		t.Fatalf("a first tip at 12%% reads speaks=%v tier=%d", speaks, tier)
	}

	// Inside the tier and inside the cooldown, it stays quiet.
	if again, _ := notify.Speaks(0.15, 0, now, now.Add(10*time.Minute)); again {
		t.Fatal("a programme flagged at 12%% spoke again at 15%% inside the hour")
	}
	// A higher tier speaks whatever the cooldown says.
	if again, tier := notify.Speaks(0.30, 0, now, now.Add(10*time.Minute)); !again || tier != 1 {
		t.Fatalf("an incident that doubled stayed quiet: speaks=%v tier=%d", again, tier)
	}
	// And the cooldown lifts on its own.
	if again, _ := notify.Speaks(0.15, 0, now, now.Add(notify.UnobservableCooldown+time.Minute)); !again {
		t.Fatal("the cooldown never lifted")
	}
	// Below the first threshold there is nothing to say.
	if speaks, tier := notify.Speaks(0.05, -1, time.Time{}, now); speaks || tier != -1 {
		t.Fatalf("5%% unobservable raised an alert: speaks=%v tier=%d", speaks, tier)
	}
}

// A channel that only wants what is burning says so once, rather than in a
// filter placed somewhere along the path.
func TestAPriorityFloorRoutes(t *testing.T) {
	t.Parallel()

	if !notify.AtLeast(notify.Critical, notify.High) {
		t.Error("a takeover did not clear a high floor")
	}
	if notify.AtLeast(notify.Low, notify.Medium) {
		t.Error("a title change cleared a medium floor")
	}
	if !notify.AtLeast(notify.Low, notify.Low) {
		t.Error("a floor refused its own level")
	}
}
