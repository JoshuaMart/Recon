package ct

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func guarded(perHour int) (*Stream, *fakeClock, Target) {
	c := &fakeClock{time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	return &Stream{MaxPerHour: perHour, Now: c.now}, c, Target{ProgramID: uuid.New(), WildcardID: uuid.New()}
}

// A write that failed must not poison the cache: the next sighting tries.
func TestAFailedWriteIsTriedAgain(t *testing.T) {
	s, _, tg := guarded(10)
	if !s.admit(tg, "k") {
		t.Fatal("first sighting refused")
	}
	s.done(tg.ProgramID, "k", false)
	if !s.admit(tg, "k") {
		t.Fatal("a name whose write failed is skipped")
	}
	s.done(tg.ProgramID, "k", true)
	if s.admit(tg, "k") {
		t.Fatal("a written name is handled again within the hour")
	}
	if s.perHour[tg.ProgramID] != 1 || s.counts.matched != 1 {
		t.Fatalf("ceiling %d, matched %d: a failed write kept its place", s.perHour[tg.ProgramID], s.counts.matched)
	}
}

// Remembered for an hour at least, two at most: an archived name's next
// certificate after that is looked at again.
func TestAWrittenNameIsLookedAtAgainAfterTheTTL(t *testing.T) {
	s, c, tg := guarded(10)
	s.admit(tg, "k")
	s.done(tg.ProgramID, "k", true)
	c.t = c.t.Add(59 * time.Minute)
	if s.admit(tg, "k") {
		t.Fatal("forgotten within the hour")
	}
	c.t = c.t.Add(2 * cacheTTL)
	if !s.admit(tg, "k") {
		t.Fatal("still remembered past two TTLs: an archived name would never be revived")
	}
}

func TestOverflowCountsNamesNotSightings(t *testing.T) {
	s, _, tg := guarded(1)
	s.admit(tg, "a")
	s.done(tg.ProgramID, "a", true)
	for range 5 {
		s.admit(tg, "b")
	}
	s.admit(tg, "c")
	if s.counts.overflow != 2 || s.apexes[tg.WildcardID].dropped != 2 {
		t.Fatalf("overflow = %d, dropped on the apex = %d, want 2 names", s.counts.overflow, s.apexes[tg.WildcardID].dropped)
	}
}

func TestCacheRotatesOnSize(t *testing.T) {
	var c cache
	now := time.Now()
	c.record("first", now)
	for i := range cacheCap {
		c.record(strconv.Itoa(i), now)
	}
	// One rotation: the first key is in the older generation, still seen.
	if !c.seen("first", now) {
		t.Fatal("a rotation by size forgot the previous generation at once")
	}
	for i := range cacheCap + 1 {
		c.record("x"+strconv.Itoa(i), now)
	}
	if c.seen("first", now) {
		t.Fatal("two rotations kept a key: memory is not bounded")
	}
}

// The same name from several logs counts once on its apex.
func TestMatchesCountEachNameOnceAnHour(t *testing.T) {
	s, c, tg := guarded(10)
	tg.Apex = "t.com"
	s.index = Index{}
	s.index.Add(tg)
	for range 3 {
		s.matches(Certificate{Names: []string{"a.t.com", "*.t.com"}})
	}
	a := s.apexes[tg.WildcardID]
	if a.names != 1 || a.wildcards != 1 {
		t.Fatalf("names %d wildcards %d, want 1 and 1", a.names, a.wildcards)
	}
	c.t = c.t.Add(3 * cacheTTL)
	s.matches(Certificate{Names: []string{"a.t.com"}})
	if a.names != 2 {
		t.Fatalf("names %d after the TTL, want 2", a.names)
	}
}
