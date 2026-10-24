package ct

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func guarded(perHour int) *Stream {
	return &Stream{MaxPerHour: perHour, Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }}
}

// A write that failed must not poison the cache: the next sighting tries.
func TestAFailedWriteIsTriedAgain(t *testing.T) {
	s, p := guarded(10), uuid.New()
	if !s.admit(p, "k") {
		t.Fatal("first sighting refused")
	}
	s.done(p, "k", false)
	if !s.admit(p, "k") {
		t.Fatal("a name whose write failed is skipped")
	}
	s.done(p, "k", true)
	if s.admit(p, "k") {
		t.Fatal("a written name is handled again")
	}
	if s.perHour[p] != 1 || s.counts.matched != 1 {
		t.Fatalf("ceiling %d, matched %d: a failed write kept its place", s.perHour[p], s.counts.matched)
	}
}

func TestOverflowCountsNamesNotSightings(t *testing.T) {
	s, p := guarded(1), uuid.New()
	s.admit(p, "a")
	s.done(p, "a", true)
	for range 5 {
		s.admit(p, "b")
	}
	s.admit(p, "c")
	if s.counts.overflow != 2 {
		t.Fatalf("overflow = %d, want 2 names", s.counts.overflow)
	}
}
