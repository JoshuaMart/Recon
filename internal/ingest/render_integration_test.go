//go:build integration

// Milestone 3, the half that is a question about an asset rather than about a
// network.
package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/scope"
)

const service = "app.acme.test:443/tcp"

// answering brings a service into existence through the ordinary path.
func (h *harness) answering(t *testing.T, c *clock, ing *ingest.Ingestor, set *scope.Set, status int, title, server string) {
	t.Helper()

	report := liveHost("app.acme.test", ingest.Port{
		Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://app.acme.test", Scheme: "https",
			StatusCode: status, Title: title, Server: server,
		},
	})
	report.Hosts[0].CDN = []ingest.CDN{{Name: "cloudflare", Type: "waf", ScanLimited: true}}
	h.walk(t, c, ing, set, 12*time.Hour, report)
}

func (h *harness) target(t *testing.T) ingest.RenderTarget {
	t.Helper()

	var id uuid.UUID
	if err := h.pool.QueryRow(context.Background(),
		`SELECT asset_id FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&id); err != nil {
		t.Fatalf("read the service: %v", err)
	}
	return ingest.RenderTarget{
		AssetID: id, OrgID: h.org, ProgramID: h.program,
		Kind: "service", Key: service, URL: "https://app.acme.test/",
		Fronted: true, Provider: "cloudflare",
	}
}

func page(status int, title, server string) *fingerprint.Result {
	return &fingerprint.Result{
		URL:     "https://app.acme.test/",
		Version: "2.1.0",
		Chain: []fingerprint.Hop{{
			URL: "https://app.acme.test/", StatusCode: status, Title: title,
			Headers: map[string]string{"Server": server}, ResponseSize: 1533,
		}},
		Technologies: []fingerprint.Technology{{Name: "nginx", Category: "web-server"}},
		Cookies:      map[string]string{"session_id": "abcdef"},
		Network:      fingerprint.Network{Host: "app.acme.test", IPs: []string{"93.184.216.34"}},
	}
}

// The rule this whole layer rests on. When no candidate produces a chain, a
// browser receiving an invalid response on a port that speaks something other
// than HTTP, the service addressed the target perfectly well and the target
// answered something that is not a page. Returning a bare error instead leaves
// the counters untouched, so the backoff never widens and the unobservable
// verdict becomes unreachable.
func TestARenderThatObtainedNoPageStillWritesAnObservation(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")

	nothing := &fingerprint.Result{URL: "https://app.acme.test/", Version: "2.1.0"}
	written, err := ing.Render(context.Background(), h.queries, h.target(t), nothing)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if written.Outcome != lifecycle.OutcomeError || written.Usable || written.Page {
		t.Fatalf("a render with no chain reads %+v", written)
	}

	if n := h.count(t,
		`SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key = $2 AND o.layer = 'fingerprint'`,
		h.program, service); n != 1 {
		t.Fatalf("%d fingerprint observations", n)
	}
	h.expectRenderReach(t, service, -1, nil)

	// And the timestamp did not move. A list showing "rendered five minutes
	// ago, no cookies" on an asset no browser ever obtained a page from is the
	// false statement the three cookie states exist to prevent.
	var last *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT last_fingerprint_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&last); err != nil {
		t.Fatalf("read last_fingerprint_at: %v", err)
	}
	if last != nil {
		t.Fatalf("last_fingerprint_at moved to %s on a render that obtained nothing", last)
	}

	// A render that got a page moves it.
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page(200, "App", "nginx")); err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(),
		`SELECT last_fingerprint_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&last); err != nil {
		t.Fatalf("read last_fingerprint_at: %v", err)
	}
	if last == nil {
		t.Fatal("a render that obtained a page left the timestamp unset")
	}
}

// Two barriers exist upstream; this is the one that makes it an impossibility
// rather than an instruction, because every write comes through it.
func TestNoScreenshotReachesTheDatabase(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")

	shot := page(200, "App", "nginx")
	shot.Screenshot = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), shot); err != nil {
		t.Fatalf("render: %v", err)
	}

	if n := h.count(t,
		`SELECT count(*) FROM observation WHERE org_id = $1 AND data ? 'screenshot'`, h.org); n != 0 {
		t.Fatalf("%d observations carry a screenshot", n)
	}
	// And nothing anywhere else either, which is the assertion rather than the
	// column: a capture in any jsonb of this database is the same problem.
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE org_id = $1 AND attributes::text LIKE '%base64%'`,
		h.org); n != 0 {
		t.Fatalf("%d projections carry base64 image data", n)
	}
}

// A mitigation aimed at the raw client is the common case, and it is the one
// the regime switch exists for. The browser clears the challenge and sees a
// 200; the raw client takes a 403 and detects it perfectly.
func TestAProtectedServiceFlipsOntoTheBrowser(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// Three concordant results in both directions, so a transient failure
	// absorbs instead of flipping the regime.
	for range lifecycle.ReachThreshold {
		h.answering(t, c, ing, set, 403, "Just a moment...", "cloudflare")
	}
	for range lifecycle.ReachThreshold {
		if _, err := ing.Render(context.Background(), h.queries, h.target(t), page(200, "Dashboard", "cloudflare")); err != nil {
			t.Fatalf("render: %v", err)
		}
	}

	var http, browser *bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT http_reachable, fingerprint_reachable FROM asset_current
		  WHERE program_id = $1 AND key = $2`, h.program, service).Scan(&http, &browser); err != nil {
		t.Fatalf("read reachability: %v", err)
	}
	if http == nil || *http {
		t.Fatalf("the raw client reads %v after three challenges", http)
	}
	if browser == nil || !*browser {
		t.Fatalf("the browser reads %v after three pages", browser)
	}

	// The target is alive throughout. A challenge must never drift an asset
	// toward a death.
	if state := h.lifecycleOf(t, service); state != lifecycle.Active {
		t.Fatalf("a protected service is %q", state)
	}
}

// Neither observer gets a result. The asset is neither alive nor dead, and
// unobservable is what says so: reading it as a death would archive every asset
// behind a mitigation that turns both of them away.
func TestNeitherObserverGettingThroughIsUnobservableAndNotADeath(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	nothing := &fingerprint.Result{URL: "https://app.acme.test/", Version: "2.1.0"}
	for range lifecycle.ReachThreshold {
		h.answering(t, c, ing, set, 403, "Just a moment...", "cloudflare")
		if _, err := ing.Render(context.Background(), h.queries, h.target(t), nothing); err != nil {
			t.Fatalf("render: %v", err)
		}
	}

	if state := h.lifecycleOf(t, service); state != lifecycle.Unobservable {
		var hs, fs int
		_ = h.pool.QueryRow(context.Background(),
			`SELECT http_streak, fingerprint_streak FROM asset_current WHERE program_id = $1 AND key = $2`,
			h.program, service).Scan(&hs, &fs)
		t.Fatalf("an asset neither observer reaches is %q (http streak %d, fingerprint streak %d)", state, hs, fs)
	}
	// It never passes through flapping on the way, and it is not a death: no
	// layer ever reported an absence.
	if state, informative, _ := h.layerOf(t, service, "http"); state == lifecycle.LayerDead || informative != 0 {
		t.Fatalf("the http layer is %q after %d informative failures", state, informative)
	}

	// One probe getting through ends it, on a single success.
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page(200, "App", "nginx")); err != nil {
		t.Fatalf("render: %v", err)
	}
	if state := h.lifecycleOf(t, service); state != lifecycle.Active {
		t.Fatalf("one observer got through and the asset is %q", state)
	}
}

// A death that was observed is not a silence. The probe reached the edge and
// the edge reported that the origin is gone, so the asset ends inactive even
// though the browser was failing at the same moment.
func TestADeadOriginDiesEvenWhileTheRenderFails(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	nothing := &fingerprint.Result{URL: "https://app.acme.test/", Version: "2.1.0"}
	for range 3 {
		h.answering(t, c, ing, set, 530, "app.acme.test | 530: Origin DNS error", "cloudflare")
		if _, err := ing.Render(context.Background(), h.queries, h.target(t), nothing); err != nil {
			t.Fatalf("render: %v", err)
		}
	}

	if state := h.lifecycleOf(t, service); state != lifecycle.Inactive {
		t.Fatalf("a service whose origin has been gone for a day is %q", state)
	}
	if state, _, _ := h.layerOf(t, service, "http"); state != lifecycle.LayerDead {
		t.Fatalf("the http layer is %q", state)
	}
}

// The instrument is dated, so a diff can tell a detection improving from an
// application changing. Two columns because one row outlives several versions.
func TestARenderCarriesTheVersionThatProducedIt(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")

	first := page(200, "App", "nginx")
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), first); err != nil {
		t.Fatalf("render: %v", err)
	}

	// A version bump that changes nothing in the result must not write a row,
	// which is the whole reason the version is stored twice.
	newer := page(200, "App", "nginx")
	newer.Version = "2.2.0"
	written, err := ing.Render(context.Background(), h.queries, h.target(t), newer)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !written.Deduplicated {
		t.Fatal("a version bump with an identical result wrote a second row")
	}

	var produced, last string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT o.producer_version, o.last_producer_version
		   FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key = $2 AND o.layer = 'fingerprint'`,
		h.program, service).Scan(&produced, &last); err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if produced != "2.1.0" || last != "2.2.0" {
		t.Fatalf("the row reads produced=%q last=%q, and the pair is what makes the diff answerable", produced, last)
	}
}

func (h *harness) expectRenderReach(t *testing.T, key string, streak int, reachable *bool) {
	t.Helper()

	var got int
	var flag *bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT fingerprint_streak, fingerprint_reachable FROM asset_current
		  WHERE program_id = $1 AND key = $2`, h.program, key).Scan(&got, &flag); err != nil {
		t.Fatalf("read the browser's reach on %s: %v", key, err)
	}
	if got != streak {
		t.Fatalf("fingerprint_streak %d, want %d", got, streak)
	}
	if reachable == nil && flag != nil {
		t.Fatalf("fingerprint_reachable is %v before three concordant results", *flag)
	}
	if reachable != nil && (flag == nil || *flag != *reachable) {
		t.Fatalf("fingerprint_reachable is %v, want %v", flag, *reachable)
	}
}

// A certain failure is not a measurement. Chrome answers ERR_UNSAFE_PORT on its
// own restricted list, so pointing a browser at one of those ports is known to
// fail before the call, and it would push an SSH service toward a state that
// qualifies what is unknown rather than what is not the web.
func TestAPortABrowserRefusesEarnsNoBaseline(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	report := liveHost("box.acme.test",
		ingest.Port{Port: 22, Protocol: "tcp", State: "open"},
		ingest.Port{Port: 8090, Protocol: "tcp", State: "open"},
	)
	h.walk(t, c, h.dated(c), set, time.Hour, report)

	if due := h.renderDueOf(t, "box.acme.test:22/tcp"); due != nil {
		t.Fatalf("port 22 is queued for a browser at %s", due)
	}
	// And the ordinary case is not filtered out with it. A forgotten
	// application on an unusual port is the reason this platform exists, so a
	// hand written list of "non web" ports would be the mistake.
	if due := h.renderDueOf(t, "box.acme.test:8090/tcp"); due == nil {
		t.Fatal("port 8090 earned no baseline, and it is exactly what a scan is for")
	}
}

// Trigger 2: a change the HTTP layer detected buys a render, and only where
// that layer is still the detector.
func TestAChangeBuysARenderOnlyWhileTheProbeIsTheDetector(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// A first contact is not a change. It has its own trigger, its own filter
	// and its own queue, and promoting it here would put every service of a
	// fresh perimeter into the queue that exists to stay short.
	h.answering(t, c, ing, set, 200, "App", "nginx")
	if priority := h.renderPriorityOf(t, service); priority != lifecycle.PriorityBaseline {
		t.Fatalf("a first contact entered at priority %d", priority)
	}

	// The page changed. That is worth a browser.
	h.answering(t, c, ing, set, 200, "App v2", "nginx")
	if priority := h.renderPriorityOf(t, service); priority != lifecycle.PriorityChange {
		t.Fatalf("a detected change entered at priority %d", priority)
	}

	// Now the raw client stops getting through. Its diff no longer buys a
	// render: the probe keeps running for reachability and for TLS, but what it
	// sees of a target refusing it is not a change worth paying a browser for.
	exec(t, h.pool, `UPDATE asset_current SET http_reachable = false,
	        fingerprint_priority = $1 WHERE program_id = $2 AND key = $3`,
		lifecycle.PriorityBaseline, h.program, service)

	h.answering(t, c, ing, set, 403, "Just a moment...", "cloudflare")
	if priority := h.renderPriorityOf(t, service); priority != lifecycle.PriorityBaseline {
		t.Fatalf("a diff from a blocked probe entered at priority %d", priority)
	}
}

func (h *harness) renderPriorityOf(t *testing.T, key string) int16 {
	t.Helper()

	var priority int16
	if err := h.pool.QueryRow(context.Background(),
		`SELECT fingerprint_priority FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&priority); err != nil {
		t.Fatalf("read the render priority of %s: %v", key, err)
	}
	return priority
}

// When the raw client is the one being turned away, the renderer is the only
// detector left and has to run at a detector's rate rather than at a
// three weekly one.
func TestTheSoleDetectorRendersAtItsOwnRate(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	for range lifecycle.ReachThreshold {
		h.answering(t, c, ing, set, 403, "Just a moment...", "cloudflare")
	}
	for range lifecycle.ReachThreshold {
		if _, err := ing.Render(context.Background(), h.queries, h.target(t), page(200, "Dashboard", "cloudflare")); err != nil {
			t.Fatalf("render: %v", err)
		}
	}

	cadence := lifecycle.DefaultCadence()
	due := h.renderDueOf(t, service)
	if due == nil {
		t.Fatal("the service has no next render")
	}
	if want := c.now.Add(cadence.RenderSole); !due.Equal(want) {
		t.Fatalf("the next render is at %s, want %s: the renderer is the only detector left", due.UTC(), want)
	}
}
