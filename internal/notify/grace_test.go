package notify_test

import (
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/notify"
)

var created = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// A failed first run resolves itself: no completed run, so the grace still
// holds. A "grace consumed" column would force deciding whether a failure
// consumes it, and that question has no good answer.
func TestAFailedFirstRunKeepsTheGrace(t *testing.T) {
	t.Parallel()

	grace := notify.Grace{AnyDiscovery: true, Assets: 4000, CreatedAt: created}
	if !grace.Active(created.Add(time.Hour)) {
		t.Fatal("a programme whose first run failed left the grace")
	}

	grace.CompletedDiscovery = true
	if grace.Active(created.Add(time.Hour)) {
		t.Fatal("a completed run did not end the grace")
	}
}

// The threshold applies to the case with no discovery only. Written as a plain
// AND it would end the grace in the middle of the run it exists for, and a
// perimeter of five thousand assets would flood with the rest.
func TestTheThresholdDoesNotEndAGraceMidRun(t *testing.T) {
	t.Parallel()

	midRun := notify.Grace{AnyDiscovery: true, Assets: 5000, CreatedAt: created}
	if !midRun.Active(created.Add(time.Hour)) {
		t.Fatal("a large perimeter left the grace partway through its own first run")
	}

	// A programme no run will ever cover is bounded by the inventory instead.
	byHand := notify.Grace{Assets: 600, CreatedAt: created}
	if byHand.Active(created.Add(time.Hour)) {
		t.Fatal("a hand fed programme stayed under grace past the threshold")
	}
	small := notify.Grace{Assets: 12, CreatedAt: created}
	if !small.Active(created.Add(time.Hour)) {
		t.Fatal("a small hand fed programme left the grace early")
	}
}

// A grace is an alert suppression mechanism, and its termination must not
// depend on any other component.
func TestTheGraceEndsAtAWeekWhateverTheRunsDid(t *testing.T) {
	t.Parallel()

	stuck := notify.Grace{AnyDiscovery: true, Assets: 4000, CreatedAt: created}
	if !stuck.Active(created.Add(6 * 24 * time.Hour)) {
		t.Fatal("the grace ended before the guardrail")
	}
	if stuck.Active(created.Add(notify.GraceAge + time.Minute)) {
		t.Fatal("a programme whose first run never finished stayed silent past a week")
	}
}

// The grace holds back new_active and nothing else. A takeover found during a
// first run is exactly the finding this product exists for.
func TestTheGraceHoldsBackOnlyTheFlood(t *testing.T) {
	t.Parallel()

	grace := notify.Grace{AnyDiscovery: true, Assets: 4000, CreatedAt: created}
	at := created.Add(time.Hour)

	if !grace.Suppresses(notify.KindNewActive, "fastrecon:crt", at) {
		t.Fatal("the flood was not held")
	}
	for _, kind := range []string{notify.KindTakeover, notify.KindPortOpened, notify.KindTechChanged} {
		if grace.Suppresses(kind, "fastrecon:crt", at) {
			t.Errorf("%s was held back by a first run grace", kind)
		}
	}
}

// A programme with no discovery holds back what was typed in, and nothing else.
// An asset found by certificate transparency under a hand entered apex was
// typed in by nobody.
func TestAHandFedGraceHoldsBackWhatWasTypedIn(t *testing.T) {
	t.Parallel()

	grace := notify.Grace{Assets: 12, CreatedAt: created}
	at := created.Add(time.Hour)

	if !grace.Suppresses(notify.KindNewActive, "manual", at) {
		t.Fatal("a typed in asset notified during its own onboarding")
	}
	if grace.Suppresses(notify.KindNewActive, "certstream", at) {
		t.Fatal("an asset nobody typed in was held back by a hand fed grace")
	}
}
