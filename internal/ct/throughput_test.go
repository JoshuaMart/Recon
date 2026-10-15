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

// The corrected assertion, made testable. "The service absorbs the full CT stream on a
// single core" names certstream-server-go, which is deployed and whose throughput is an
// image's property. Recon owns the matcher: decoding a frame and walking every SAN
// through the set, on one goroutine. The decode is inside the clock: it was outside
// once, and that hid the larger half of the cost.
//
// There is no queue between socket and walk, the strongest form of the assertion: the
// read loop calls the matcher synchronously, so one that could not keep up would apply
// backpressure to the socket instead of growing an unbounded buffer.
func TestTheMatcherOutrunsTheStreamOnOneGoroutine(t *testing.T) {
	frames := fixture(t)
	raw := rawFixture(t)
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
	var (
		decoder ct.Decoder
		frame   ct.Frame
	)
	started := time.Now()
	matched := 0
	for range rounds {
		for _, line := range raw {
			if err := decoder.Decode(line, &frame); err != nil {
				t.Fatalf("decode: %v", err)
			}
			found, _ := set.Sightings(frame.Data.LeafCert.AllDomains)
			matched += len(found)
		}
	}
	elapsed := time.Since(started)

	handled := rounds * len(raw)
	rate := float64(handled) / elapsed.Seconds()

	// The feed delivers about two thousand certificates a second at steady state and about
	// four thousand while catching up, both measured. The floor is an order of magnitude
	// above that and one below what a laptop measured, failing on a regression and not a
	// slow machine.
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

// The walk is read-only and the set is swapped whole, which lets the stream read it
// without a lock. The only shared mutable state is the pointer (the map is never written
// after construction), which this exercises under the race detector. The failure it
// prevents is silent: a map written under a walk gives wrong matches, not an error.
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
