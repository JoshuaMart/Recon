package fingerprint_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/fingerprint"
)

// The link local range is first because it is the one that costs a compromise
// rather than an embarrassment: cloud instance credentials live at
// 169.254.169.254.
func TestTheRangesABrowserIsNeverPointedAt(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"169.254.169.254", "127.0.0.1", "10.1.2.3", "172.20.0.5",
		"192.168.1.1", "0.0.0.0", "100.64.3.4", "::1", "fe80::1", "fd00::1",
		// The mapped form. Without unwrapping it first, every rule above is
		// walked around by writing the same address the other way, and the
		// check that looks most obviously correct is the one that does nothing.
		"::ffff:169.254.169.254",
	}
	for _, raw := range blocked {
		if !fingerprint.Internal(netip.MustParseAddr(raw)) {
			t.Errorf("%s would be handed to a browser", raw)
		}
	}

	for _, raw := range []string{"93.184.216.34", "1.1.1.1", "2606:4700::1111"} {
		if fingerprint.Internal(netip.MustParseAddr(raw)) {
			t.Errorf("%s was refused, and it is a perfectly ordinary target", raw)
		}
	}
}

func TestTheGuardResolvesANameRatherThanTrustingIt(t *testing.T) {
	t.Parallel()

	guard := fingerprint.NewGuard(func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "metadata.internal":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "acme.test":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return nil, http.ErrNoLocation
	})
	ctx := context.Background()

	// An inventory holds names, and a name pointing at an internal address is
	// exactly the case worth catching: the literal forms are the ones nobody
	// writes by accident.
	if err := guard.Check(ctx, "https://metadata.internal/latest/"); err == nil {
		t.Fatal("a name resolving to the metadata service was accepted")
	}
	if err := guard.Check(ctx, "https://acme.test/"); err != nil {
		t.Fatalf("an ordinary name was refused: %v", err)
	}
	if err := guard.Check(ctx, "http://[::ffff:127.0.0.1]/"); err == nil {
		t.Fatal("the mapped loopback literal was accepted")
	}

	// A resolution that fails is not a refusal. The browser does its own, and
	// turning a DNS failure here into one would silently drop every render on a
	// host whose resolver is having a bad minute.
	if err := guard.Check(ctx, "https://unknown.test/"); err != nil {
		t.Fatalf("a name that would not resolve was refused: %v", err)
	}
}

// A 429 is a state of the service, so it must not touch the asset. The caller
// has to be able to tell it from every other failure, which is why it is a type
// rather than a status code the call site re-reads.
func TestSaturationIsItsOwnAnswerAndItsWaitIsSpread(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := fingerprint.New(server.URL, 5*time.Second, permissive())

	seen := map[time.Duration]struct{}{}
	for range 20 {
		_, err := client.Scan(context.Background(), "https://acme.test/", fingerprint.Options{})
		busy, ok := err.(*fingerprint.Saturated)
		if !ok {
			t.Fatalf("a 429 came back as %v", err)
		}
		// Everyone refused at the same instant received the same value, and
		// waiting exactly that long reconstitutes the convoy the refusal was
		// meant to break.
		if busy.RetryAfter < 5*time.Second || busy.RetryAfter > 15*time.Second {
			t.Fatalf("the wait is %s, outside half to one and a half of 10s", busy.RetryAfter)
		}
		seen[busy.RetryAfter] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("twenty refusals produced one wait, so the spread is not spreading")
	}
}

// The service could not address the target at all. That is a probe error and
// not a measurement, and the two are handled in opposite ways.
func TestATargetTheServiceWouldNotAddressIsARefusal(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	client := fingerprint.New(server.URL, 5*time.Second, permissive())
	_, err := client.Scan(context.Background(), "https://acme.test/", fingerprint.Options{})
	if err == nil {
		t.Fatal("a failing service answered with a result")
	}
	if _, busy := err.(*fingerprint.Saturated); busy {
		t.Fatal("a 500 was read as saturation, which would give the budget back for a call that happened")
	}
}

// Chromium's own list rather than a hand written list of "non web" ports. The
// difference is the whole point: a hand written one would be wrong about the
// forgotten application on 9000 that is exactly what this platform exists to
// find.
func TestTheBaselineFilterReadsTheInstrument(t *testing.T) {
	t.Parallel()

	for _, port := range []int{22, 25, 139, 6667} {
		if fingerprint.Renderable(port) {
			t.Errorf("port %d earns a browser that will answer ERR_UNSAFE_PORT", port)
		}
	}
	for _, port := range []int{80, 443, 8080, 8090, 9000, 3306, 6379, 9200} {
		if !fingerprint.Renderable(port) {
			t.Errorf("port %d was filtered out, and a certain failure is not what this reads", port)
		}
	}
}

// A guard that accepts everything, so a client test measures the client.
func permissive() *fingerprint.Guard {
	return fingerprint.NewGuard(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
}

// Chrome still parses the legacy inet_aton spellings, so a guard that only
// knows the dotted quad lets them past to a resolver that fails on them and
// then reads the failure as "not my problem".
func TestTheLegacyAddressSpellingsAreStillAddresses(t *testing.T) {
	t.Parallel()

	guard := fingerprint.NewGuard(func(context.Context, string) ([]netip.Addr, error) {
		return nil, http.ErrNoLocation
	})
	ctx := context.Background()

	loopback := []string{
		"http://2130706433/", // decimal
		"http://0177.0.0.1/", // octal
		"http://0x7f.0.0.1/", // hexadecimal
		"http://127.1/",      // two parts, the last absorbing three bytes
		"http://2852039166/", // 169.254.169.254 in decimal
	}
	for _, raw := range loopback {
		if err := guard.Check(ctx, raw); err == nil {
			t.Errorf("%s was accepted, and a browser reads it as an internal address", raw)
		}
	}

	// An ordinary name that happens not to resolve is still not a refusal.
	if err := guard.Check(ctx, "https://acme.test/"); err != nil {
		t.Errorf("an ordinary name was refused: %v", err)
	}
	// And a public address in a legacy spelling is still public.
	if err := guard.Check(ctx, "http://3221226219/"); err != nil {
		t.Errorf("192.0.2.235 written in decimal was refused: %v", err)
	}
}
