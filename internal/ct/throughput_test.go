package ct_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ct"
)

// The corrected milestone assertion, and the correction is what makes it
// testable here.
//
// "The service absorbs the full CT stream on a single core" names
// certstream-server-go, which is deployed rather than written, so its throughput
// is a property of an image. What Recon owns is the matcher: decoding a frame
// and walking every SAN of it through the set, on one goroutine.
//
// There is no queue between the socket and the walk to measure, and that is the
// strongest form of the assertion rather than a gap in it: the read loop calls
// the matcher synchronously, so a matcher that could not keep up would apply
// backpressure to the socket instead of growing a buffer nobody bounded.
func TestTheMatcherOutrunsTheStreamOnOneGoroutine(t *testing.T) {
	frames := fixture(t)
	if len(frames) < 50 {
		t.Fatalf("the fixture holds %d frames", len(frames))
	}

	// An apex under every SAN of the corpus, so the walk does the most work it
	// can rather than missing on the first lookup.
	claims := make([]ct.Claim, 0, 64)
	for _, f := range frames {
		for _, san := range f.Data.LeafCert.AllDomains {
			claims = append(claims, ct.Claim{
				OrgID: uuid.New(), ProgramID: uuid.New(), Apex: trimWildcard(san),
			})
		}
	}
	set := ct.NewSet(claims)

	const rounds = 40
	started := time.Now()
	matched := 0
	for range rounds {
		for _, f := range frames {
			found, _ := set.Sightings(f.Data.LeafCert.AllDomains)
			matched += len(found)
		}
	}
	elapsed := time.Since(started)

	handled := rounds * len(frames)
	rate := float64(handled) / elapsed.Seconds()

	// The feed delivers about two thousand certificates a second at steady
	// state and around four thousand while it catches up, both measured. The
	// floor here is an order of magnitude above that and an order of magnitude
	// below what was measured on a laptop, so it fails on a regression rather
	// than on a slow machine.
	const floor = 20_000
	if rate < floor {
		t.Errorf("the matcher walks %.0f frames a second, and the feed delivers about 2 000: "+
			"below %d there is no margin left to lose", rate, floor)
	}
	if matched == 0 {
		t.Fatal("nothing matched, so this measured a map lookup that always misses")
	}
	t.Logf("%.0f frames/s on one goroutine, %d sightings", rate, matched/rounds)
}

func trimWildcard(san string) string {
	if len(san) > 2 && san[0] == '*' && san[1] == '.' {
		return san[2:]
	}
	return san
}

// The walk is read-only and the set is swapped whole rather than mutated, which
// is what lets the stream read it without a lock.
//
// The shared mutable state is exactly one thing, the pointer, because the map
// behind it is never written after it is built. So that is what this exercises,
// under the race detector, and the failure it prevents is the silent kind: a map
// written under a walk produces wrong matches rather than an error.
func TestTheSetIsSafeToReadWhileItIsReplaced(t *testing.T) {
	m := ct.New(nil, nil, nil, ct.DefaultOptions(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = m.Apexes()
		}
	}()

	for range 500 {
		m.Swap(ct.NewSet([]ct.Claim{{
			OrgID: uuid.New(), ProgramID: uuid.New(), Apex: "acme.test",
		}}))
		m.Swap(ct.NewSet(nil))
	}
	cancel()
	<-done
}
