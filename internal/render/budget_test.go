package render_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/render"
)

// The assertion the whole meter exists for. A pass over a large batch has to
// come out under the programme's published rate, and the only way to check that
// is to count what the meter let through against what the clock allowed.
func TestAPassOverFiveHundredAssetsStaysUnderTheRateLimit(t *testing.T) {
	t.Parallel()

	const (
		rps      = 10
		cost     = 30
		assets   = 500
		duration = time.Hour
	)

	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	budget := render.NewBudget(cost, func() time.Time { return now })
	program := uuid.New()

	// An hour of wall clock, sampled every second, with the pass trying every
	// asset it has on each tick.
	allowed := 0
	for range int(duration / time.Second) {
		for range assets {
			if !budget.Charge(program, rps) {
				break
			}
			allowed++
		}
		now = now.Add(time.Second)
	}

	// One render costs thirty requests, so ten a second buys one render every
	// three seconds and no more.
	ceiling := int(duration/time.Second) * rps / cost
	if allowed > ceiling+1 {
		t.Fatalf("%d renders in an hour, and the programme's rate allows %d", allowed, ceiling)
	}
	if allowed < ceiling-2 {
		t.Fatalf("%d renders in an hour, well under the %d the programme allows: the meter is not refilling", allowed, ceiling)
	}
}

// An idle programme must not accumulate a burst it can spend all at once on
// somebody's server, which is what a rate limit is for.
func TestAnIdleProgrammeDoesNotBankABurst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	budget := render.NewBudget(30, func() time.Time { return now })
	program := uuid.New()

	now = now.Add(24 * time.Hour)
	burst := 0
	for range 100 {
		if !budget.Charge(program, 10) {
			break
		}
		burst++
	}
	if burst != 1 {
		t.Fatalf("a day of idling banked %d renders", burst)
	}
}

// A saturated service means nothing reached the target, so the charge returns.
// Without it a busy renderer would throttle a programme's real renders on
// behalf of renders that never happened.
func TestARefusalGivesTheBudgetBack(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	budget := render.NewBudget(30, func() time.Time { return now })
	program := uuid.New()

	if !budget.Charge(program, 10) {
		t.Fatal("the first render was refused")
	}
	if budget.Charge(program, 10) {
		t.Fatal("a second render went through without a second's worth of budget")
	}

	budget.Refund(program)
	if !budget.Charge(program, 10) {
		t.Fatal("the refunded charge was not available again")
	}
}
