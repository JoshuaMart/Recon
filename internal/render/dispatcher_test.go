package render

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTargetURL(t *testing.T) {
	for _, tc := range []struct {
		t    Target
		want string
	}{
		{Target{Kind: "service", Host: "a.example.com", Port: 443, Scheme: "https"}, "https://a.example.com/"},
		{Target{Kind: "service", Host: "a.example.com", Port: 80, Scheme: "http"}, "http://a.example.com/"},
		{Target{Kind: "service", Host: "a.example.com", Port: 8443, Scheme: "https"}, "https://a.example.com:8443/"},
		{Target{Kind: "service", Host: "a.example.com", Port: 80}, "http://a.example.com/"},
		{Target{Kind: "service", Host: "a.example.com", Port: 9000}, "https://a.example.com:9000/"},
		{Target{Kind: "url", Key: "https://a.example.com/app", Host: "a.example.com", Port: 443}, "https://a.example.com/app"},
	} {
		if got := tc.t.URL(); got != tc.want {
			t.Errorf("URL(%+v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestPoliteness(t *testing.T) {
	d := &Dispatcher{Max: 50}
	d.init()
	host := func(h string) Target { return Target{AssetID: uuid.New(), Host: h} }
	first, second := host("a.example.com"), host("a.example.com")
	if !d.claim(first) || !d.claim(second) {
		t.Fatal("two renders per host are allowed")
	}
	if d.claim(host("a.example.com")) {
		t.Error("a third render on one host was claimed")
	}
	if !d.claim(host("b.example.com")) || !d.claim(host("c.example.com")) {
		t.Fatal("other hosts of the domain up to four")
	}
	if d.claim(host("d.example.com")) {
		t.Error("a fifth render on one registrable domain was claimed")
	}
	if !d.claim(host("a.example.co.uk")) {
		t.Error("another registrable domain is not throttled by this one")
	}
	same := host("e.other.com")
	if !d.claim(same) || d.claim(same) {
		t.Error("an asset in flight is claimed once")
	}
	d.release(same)
	// The pass read the queue before this render was written: it would
	// render the asset twice.
	if d.claim(same) {
		t.Error("an asset released during a pass was claimed again by it")
	}
	d.mu.Lock()
	clear(d.released)
	d.mu.Unlock()
	if !d.claim(same) {
		t.Error("a released asset can be claimed again by the next pass")
	}
}

func TestRetryDoublesUpToThePeriodicCadence(t *testing.T) {
	w := &Writer{Settings: DefaultSettings}
	for failures, want := range map[int]time.Duration{
		1: 6 * time.Hour, 2: 12 * time.Hour, 3: 24 * time.Hour, 5: 96 * time.Hour,
		7: 16 * 24 * time.Hour, 8: 21 * 24 * time.Hour, 30000: 21 * 24 * time.Hour,
	} {
		if got := w.retryAfter(failures); got != want {
			t.Errorf("retryAfter(%d) = %s, want %s", failures, got, want)
		}
	}
}
