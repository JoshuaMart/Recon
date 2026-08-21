//go:build integration

// Milestone 2, in the assertions a database can answer.
//
// Every transition here is asserted on a controlled clock. The thresholds are
// "three failures over at least twenty four hours", and a test that lets real
// time pass measures nothing: three failures inside ninety minutes is the case
// that separates a disappearance from an outage, and it cannot be written
// without owning the clock.
package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/signals"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// dated builds an ingestor whose clock and jitter a test owns.
func (h *harness) dated(c *clock) *ingest.Ingestor {
	return ingest.New(nil, lifecycle.DefaultCadence(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		ingest.WithClock(c.Now),
		// Zero jitter, so a due date is a value rather than a window. The
		// spread itself is asserted where it is computed.
		ingest.WithJitter(func() float64 { return 0 }))
}

func liveHost(host string, ports ...ingest.Port) ingest.Report {
	return ingest.Report{
		SchemaVersion: "1.0",
		Run:           ingest.RunInfo{Input: "targets", Completed: true, Version: "1.2.3"},
		Hosts: []ingest.Host{{
			Host: host, Status: ingest.StatusLive,
			Addresses: []string{"93.184.216.34"}, Ports: ports,
		}},
	}
}

func deadHost(host, reason string, cnames ...string) ingest.Report {
	return ingest.Report{
		SchemaVersion: "1.0",
		Run:           ingest.RunInfo{Input: "targets", Completed: true, Version: "1.2.3"},
		Hosts: []ingest.Host{{
			Host: host, Status: ingest.StatusDead, Reason: reason, CNAME: cnames,
		}},
	}
}

func (h *harness) lifecycleOf(t *testing.T, key string) string {
	t.Helper()

	var state string
	err := h.pool.QueryRow(context.Background(),
		`SELECT lifecycle FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&state)
	if err != nil {
		t.Fatalf("read lifecycle of %s: %v", key, err)
	}
	return state
}

func (h *harness) layerOf(t *testing.T, key, layer string) (string, int, int) {
	t.Helper()

	var state string
	var informative, nonInformative int
	err := h.pool.QueryRow(context.Background(),
		`SELECT l.state, l.informative_failures, l.non_informative_failures
		   FROM asset_layer l JOIN asset a ON a.id = l.asset_id
		  WHERE a.program_id = $1 AND a.key = $2 AND l.layer = $3`,
		h.program, key, layer).Scan(&state, &informative, &nonInformative)
	if err != nil {
		t.Fatalf("read %s layer of %s: %v", layer, key, err)
	}
	return state, informative, nonInformative
}

// walk replays one report at a fixed cadence.
func (h *harness) walk(t *testing.T, c *clock, ing *ingest.Ingestor, set *scope.Set, gap time.Duration, reports ...ingest.Report) {
	t.Helper()

	ctx := context.Background()
	for _, report := range reports {
		if _, err := ing.Report(ctx, h.queries, h.run(), set, report); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		c.now = c.now.Add(gap)
	}
}

func TestAnNXDomainConfirmedOverADayIsADeath(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	gone := deadHost("gone.acme.test", ingest.ReasonNXDomain)
	h.walk(t, c, h.dated(c), set, 12*time.Hour, gone, gone, gone)

	if state := h.lifecycleOf(t, "gone.acme.test"); state != lifecycle.Inactive {
		t.Fatalf("three nxdomains spread over 24 h left the asset %q", state)
	}
	if state, informative, _ := h.layerOf(t, "gone.acme.test", "dns"); state != lifecycle.LayerDead || informative != 3 {
		t.Fatalf("the dns layer is %q after %d informative failures", state, informative)
	}
}

// The floor is the assertion, not the count. Without it the threshold is not a
// threshold: the backoff curve can deliver three failures inside ninety
// minutes, which is not long enough to tell an outage from a disappearance.
func TestAnNXDomainConfirmedInNinetyMinutesIsNot(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	gone := deadHost("blip.acme.test", ingest.ReasonNXDomain)
	h.walk(t, c, h.dated(c), set, 45*time.Minute, gone, gone, gone)

	if state := h.lifecycleOf(t, "blip.acme.test"); state != lifecycle.Flapping {
		t.Fatalf("three nxdomains inside 90 min left the asset %q", state)
	}
}

// A timeout is indistinguishable from a filter or a ban. An inventory that
// reads it as a death archives every host behind a firewall that started
// dropping.
func TestARepeatedTimeoutNeverProducesAnInactive(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	silent := deadHost("silent.acme.test", ingest.ReasonTimeout)
	h.walk(t, c, h.dated(c), set, 24*time.Hour, silent, silent, silent, silent, silent, silent)

	if state := h.lifecycleOf(t, "silent.acme.test"); state == lifecycle.Inactive {
		t.Fatal("six timeouts spread over six days produced a death")
	}
	if _, informative, nonInformative := h.layerOf(t, "silent.acme.test", "dns"); informative != 0 || nonInformative != 6 {
		t.Fatalf("the counters read informative=%d non_informative=%d, and only the second may move on a timeout",
			informative, nonInformative)
	}
}

// A resolver pool that failed validation turns every dead host into a live one
// or every live host into a timeout, and those are the two signals a death is
// read from. A degraded observer is an observer, not a verdict.
func TestADegradedRunCannotConcludeADeath(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	gone := deadHost("unsure.acme.test", ingest.ReasonNXDomain)
	gone.Degraded = []string{ingest.DegradedResolversUnvalidated}
	h.walk(t, c, h.dated(c), set, 12*time.Hour, gone, gone, gone)

	if state := h.lifecycleOf(t, "unsure.acme.test"); state == lifecycle.Inactive {
		t.Fatal("a run that said it could not vouch for its resolvers still archived a host")
	}
	if _, informative, nonInformative := h.layerOf(t, "unsure.acme.test", "dns"); informative != 0 || nonInformative != 3 {
		t.Fatalf("a degraded nxdomain counted as informative=%d non_informative=%d", informative, nonInformative)
	}
}

func TestAnAssetComesBackOnASingleSuccess(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	gone := deadHost("back.acme.test", ingest.ReasonNXDomain)
	h.walk(t, c, ing, set, 12*time.Hour, gone, gone)
	if state := h.lifecycleOf(t, "back.acme.test"); state != lifecycle.Flapping {
		t.Fatalf("two failures left the asset %q", state)
	}

	h.walk(t, c, ing, set, 12*time.Hour, liveHost("back.acme.test"))
	if state := h.lifecycleOf(t, "back.acme.test"); state != lifecycle.Active {
		t.Fatalf("a single success left the asset %q, and the threshold asks for one", state)
	}
	if _, informative, _ := h.layerOf(t, "back.acme.test", "dns"); informative != 0 {
		t.Fatalf("the failure run survived a success: %d", informative)
	}
}

// A name that exists without an address, an MX-only host or a TXT validation
// record, is not a name that does not exist. Confusing the two would delete
// every mail host from an inventory.
func TestNoDataIsNotNXDomain(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	mail := deadHost("mx.acme.test", ingest.ReasonNoAnswer)
	h.walk(t, c, h.dated(c), set, 12*time.Hour, mail, mail, mail)

	if state := h.lifecycleOf(t, "mx.acme.test"); state != lifecycle.Active {
		t.Fatalf("a name answering NOERROR with an empty section is %q", state)
	}
}

// Silence is not a measurement, and turning it into one is the single most
// expensive mistake available here.
func TestAHostATruncatedRunNeverReachedKeepsItsDueDate(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	h.walk(t, c, ing, set, time.Hour, liveHost("kept.acme.test"))
	before := h.dueOf(t, "kept.acme.test")

	truncated := ingest.Report{
		SchemaVersion: "1.0",
		Run: ingest.RunInfo{
			Input: "targets", Completed: false, TruncatedByTimeout: true, Version: "1.2.3",
		},
		Hosts: []ingest.Host{{Host: "kept.acme.test", Status: ingest.StatusDiscovered}},
	}
	h.walk(t, c, ing, set, time.Hour, truncated)

	if after := h.dueOf(t, "kept.acme.test"); !after.Equal(before) {
		t.Fatalf("the due date moved from %s to %s on a host the run never reached", before, after)
	}
	if n := h.count(t,
		`SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.key = 'kept.acme.test' AND a.program_id = $1`, h.program); n != 1 {
		t.Fatalf("%d observations, and the second report answered for nothing", n)
	}
}

func (h *harness) dueOf(t *testing.T, key string) time.Time {
	t.Helper()

	var due time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT next_resolve_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&due)
	if err != nil {
		t.Fatalf("read due date of %s: %v", key, err)
	}
	return due
}

// A run's scope decides which dates its report moves. An asset due for full
// does not need a resolve run, because full runs every rung below it, and the
// reverse is not true: a resolve run learns nothing about a port.
func TestAResolveRunDoesNotSatisfyAFullOne(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	full := h.run()
	full.Scope = lifecycle.RungFull
	if _, err := ing.Report(context.Background(), h.queries, full, set, liveHost("ladder.acme.test")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	firstFull := h.fullDueOf(t, "ladder.acme.test")

	c.now = c.now.Add(2 * time.Hour)
	resolve := h.run()
	resolve.Scope = lifecycle.RungResolve
	if _, err := ing.Report(context.Background(), h.queries, resolve, set, liveHost("ladder.acme.test")); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if after := h.fullDueOf(t, "ladder.acme.test"); !after.Equal(firstFull) {
		t.Fatalf("a resolve run moved the full due date from %s to %s", firstFull, after)
	}
	if due := h.dueOf(t, "ladder.acme.test"); !due.Equal(c.now.Add(24 * time.Hour)) {
		t.Fatalf("the resolve date is %s, want %s", due, c.now.Add(24*time.Hour))
	}
}

func (h *harness) fullDueOf(t *testing.T, key string) time.Time {
	t.Helper()

	var due time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT next_full_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&due)
	if err != nil {
		t.Fatalf("read full due date of %s: %v", key, err)
	}
	return due
}

// A bare counter would have forced rewriting the probe the day the alert needs
// to carry what is vulnerable and on what evidence. The timestamp is added at
// ingestion, because a date inside the payload would differ on every pass and
// defeat deduplication on exactly the assets worth following.
func TestADanglingCNAMEIsRecordedWithItsEvidence(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	orphan := deadHost("old.acme.test", ingest.ReasonNXDomain, "old.acme.test", "bucket.s3.example.net.")
	h.walk(t, c, ing, set, time.Hour, orphan)

	finding := h.takeoverOf(t, "old.acme.test")
	if finding["kind"] != signals.KindOrphanCNAME {
		t.Fatalf("kind is %v", finding["kind"])
	}
	if finding["target"] != "bucket.s3.example.net" {
		t.Fatalf("target is %v", finding["target"])
	}
	if finding["signature"] != "nxdomain" {
		t.Fatalf("signature is %v", finding["signature"])
	}
	detected, ok := finding["detected_at"].(string)
	if !ok || detected == "" {
		t.Fatalf("detected_at is %v, and it is what phase 5 alerts on", finding["detected_at"])
	}

	// Confirmed hourly, the finding must keep the instant it was first seen.
	// A date that moved would rewrite the row on every pass and take
	// deduplication to zero on the assets worth following.
	h.walk(t, c, ing, set, time.Hour, orphan, orphan)
	again := h.takeoverOf(t, "old.acme.test")
	if again["detected_at"] != detected {
		t.Fatalf("detected_at moved from %v to %v across confirmations", detected, again["detected_at"])
	}

	// And it goes when the name resolves again, cleared by the layer that
	// found it.
	h.walk(t, c, ing, set, time.Hour, liveHost("old.acme.test"))
	if h.hasTakeover(t, "old.acme.test") {
		t.Fatal("the finding survived the name coming back")
	}
}

func (h *harness) takeoverOf(t *testing.T, key string) map[string]any {
	t.Helper()

	var raw []byte
	err := h.pool.QueryRow(context.Background(),
		`SELECT attributes -> 'takeover_candidate' FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&raw)
	if err != nil {
		t.Fatalf("read takeover candidate of %s: %v", key, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode takeover candidate: %v", err)
	}
	return out
}

func (h *harness) hasTakeover(t *testing.T, key string) bool {
	t.Helper()

	var present bool
	err := h.pool.QueryRow(context.Background(),
		`SELECT attributes ? 'takeover_candidate' FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&present)
	if err != nil {
		t.Fatalf("read attributes of %s: %v", key, err)
	}
	return present
}

// The presence of a response is not liveness. On a fronted target the edge
// always answers, so an asset whose origin is dead would stay active forever
// under any rule that reads the transport.
func TestADeadOriginBehindALiveEdgeStillDies(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	fronted := liveHost("shop.acme.test", ingest.Port{
		Port: 443, Protocol: "tcp", State: "open",
		HTTP: &ingest.HTTP{
			URL: "https://shop.acme.test", Scheme: "https", StatusCode: 530,
			Server: "cloudflare", Title: "shop.acme.test | 530: Origin DNS error",
		},
	})
	fronted.Hosts[0].CDN = []ingest.CDN{{Name: "cloudflare", Type: "waf", ScanLimited: true}}

	h.walk(t, c, h.dated(c), set, 12*time.Hour, fronted, fronted, fronted)

	service := "shop.acme.test:443/tcp"
	if state := h.lifecycleOf(t, service); state != lifecycle.Inactive {
		t.Fatalf("the service is %q while its origin has been gone for a day and a half", state)
	}
	if state := h.lifecycleOf(t, "shop.acme.test"); state != lifecycle.Active {
		t.Fatalf("the name is %q, and the name still resolves", state)
	}

	var fronting bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT is_cdn FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&fronting); err != nil {
		t.Fatalf("read is_cdn: %v", err)
	}
	if !fronting {
		t.Error("the service was not marked as sitting behind an edge")
	}
}

// A derived service is a candidate until something addresses the service
// itself. The port scan is an observation about the host, and reading it as one
// about the service would report every open port as a verified application.
func TestAnOpenPortBecomesACandidateAndAnAnsweringOneBecomesActive(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	report := liveHost("box.acme.test",
		ingest.Port{Port: 22, Protocol: "tcp", State: "open"},
		ingest.Port{Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://box.acme.test", Scheme: "https", StatusCode: 200, Title: "Home",
			Server: "nginx", Tech: []string{"nginx"},
		}},
	)
	h.walk(t, c, ing, set, time.Hour, report)

	if state := h.lifecycleOf(t, "box.acme.test:22/tcp"); state != lifecycle.Candidate {
		t.Fatalf("a port nothing spoke to is %q", state)
	}
	if state := h.lifecycleOf(t, "box.acme.test:443/tcp"); state != lifecycle.Active {
		t.Fatalf("a service that answered is %q", state)
	}

	var status int
	var title, server string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status_code, title, server FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, "box.acme.test:443/tcp").Scan(&status, &title, &server); err != nil {
		t.Fatalf("read promoted columns: %v", err)
	}
	if status != 200 || title != "Home" || server != "nginx" {
		t.Fatalf("the promoted columns read %d %q %q", status, title, server)
	}

	// The service earned its baseline by answering, and it enters low.
	var due time.Time
	var priority int16
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_fingerprint_at, fingerprint_priority FROM asset_current
		  WHERE program_id = $1 AND key = $2`,
		h.program, "box.acme.test:443/tcp").Scan(&due, &priority); err != nil {
		t.Fatalf("read the render schedule: %v", err)
	}
	if priority != lifecycle.PriorityBaseline {
		t.Errorf("a baseline entered at priority %d", priority)
	}
}

// A host answering on a quarter of the curated list does not have twenty-five
// services: it is a tarpit, a device accepting every connection, or an edge
// answering for everything behind it.
func TestTwentyFiveOpenPortsDeriveNothing(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	var ports []ingest.Port
	for port := 8000; port < 8025; port++ {
		ports = append(ports, ingest.Port{Port: port, Protocol: "tcp", State: "open"})
	}
	h.walk(t, c, h.dated(c), set, time.Hour, liveHost("tarpit.acme.test", ports...))

	if n := h.count(t,
		`SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'service'`, h.program); n != 0 {
		t.Fatalf("%d services were derived from a host answering on 25 ports", n)
	}
	// The observation keeps its full port list either way, so the finding
	// stays readable even when it does not become an asset.
	var open int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT jsonb_array_length(o.data -> 'open_ports')
		   FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key = 'tarpit.acme.test' AND o.layer = 'tcp'`,
		h.program).Scan(&open); err != nil {
		t.Fatalf("read the port list: %v", err)
	}
	if open != 25 {
		t.Fatalf("the observation carries %d ports", open)
	}
}

// A report that lists only open ports cannot conclude a death: an empty list is
// "nothing was open" and "nothing was tried" at once, and those are opposite
// findings.
func TestTheTCPLayerConcludesOnlyWhatTheSweepCounted(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	silent := liveHost("closed.acme.test")
	h.walk(t, c, ing, set, time.Hour, silent)
	if n := h.count(t,
		`SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key = 'closed.acme.test' AND o.layer = 'tcp'`,
		h.program); n != 0 {
		t.Fatalf("%d tcp observations from a report that counted nothing", n)
	}

	refused := liveHost("closed.acme.test")
	refused.Hosts[0].Scan = &ingest.Scan{Scanned: 32, Open: 0, Refused: 32}
	h.walk(t, c, ing, set, 12*time.Hour, refused, refused, refused)

	if state, informative, _ := h.layerOf(t, "closed.acme.test", "tcp"); state != lifecycle.LayerDead || informative != 3 {
		t.Fatalf("a host refusing everything for 24 h reads %q after %d informative failures", state, informative)
	}

	// One filtered port breaks it: what is filtered is indistinguishable from
	// what is banned, and a service could be sitting behind it.
	filtered := liveHost("maybe.acme.test")
	filtered.Hosts[0].Scan = &ingest.Scan{Scanned: 32, Open: 0, Refused: 31, Filtered: 1}
	h.walk(t, c, ing, set, 12*time.Hour, filtered, filtered, filtered)

	if state, informative, nonInformative := h.layerOf(t, "maybe.acme.test", "tcp"); state == lifecycle.LayerDead {
		t.Fatalf("a sweep with a filtered port concluded a death: %q informative=%d non_informative=%d",
			state, informative, nonInformative)
	}
}

func TestAHandEnteredHostIsDueForFullOnItsFirstRun(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	entered, err := ing.Enter(context.Background(), h.queries, h.run(), set,
		[]string{"typed.acme.test", "not-a-host", "outside.example.org"})
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	if len(entered.Accepted) != 2 || len(entered.Refused) != 1 {
		t.Fatalf("%d accepted and %d refused", len(entered.Accepted), len(entered.Refused))
	}

	// Somebody typed it in to find out what it exposes. A resolution would
	// only report that the name answers, and the ladder makes the expensive
	// rung free to ask for: full runs every rung below it.
	full := h.fullDueOf(t, "typed.acme.test")
	if !full.Equal(c.now) {
		t.Fatalf("the full due date is %s, want %s", full, c.now)
	}

	// Stored and never probed. An entry outside the perimeter is kept and
	// displayed, which is what makes a scope mistake visible instead of silent.
	var due *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_full_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, "outside.example.org").Scan(&due); err != nil {
		t.Fatalf("read the outside entry: %v", err)
	}
	if due != nil {
		t.Fatalf("an out of scope entry was scheduled for %s", due)
	}
}

func TestAHundredHandEnteredNamesProduceAnInventory(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}

	entries := make([]string, 0, 100)
	for n := range 100 {
		entries = append(entries, fmt.Sprintf("host-%02d.acme.test", n))
	}

	entered, err := h.dated(c).Enter(context.Background(), h.queries, h.run(), set, entries)
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	if len(entered.Accepted) != 100 {
		t.Fatalf("%d of 100 entries were accepted", len(entered.Accepted))
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset_current
		  WHERE program_id = $1 AND scope_status = 'in_scope' AND next_full_at IS NOT NULL`,
		h.program); n != 100 {
		t.Fatalf("%d of 100 names are scheduled for a full run", n)
	}
	if n := h.count(t,
		`SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id WHERE a.program_id = $1`,
		h.program); n != 0 {
		t.Fatalf("%d observations, and nothing has looked at any of them yet", n)
	}
}

// This is the one case where a path is an identity rather than the place a
// redirect landed. A URL has no liveness of its own: what answers is the
// service, so the URL earns its render once the service has answered, and the
// renderer is given the URL as declared rather than the service root.
func TestAHandEnteredURLEarnsItsRenderOnlyOnceItsServiceAnswered(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	entered, err := ing.Enter(context.Background(), h.queries, h.run(), set,
		[]string{"https://portal.acme.test/admin/login"})
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	if len(entered.Accepted) != 3 {
		t.Fatalf("a URL produced %d assets, and it needs its host, its service and itself", len(entered.Accepted))
	}

	const declared = "https://portal.acme.test/admin/login"
	if due := h.renderDueOf(t, declared); due != nil {
		t.Fatalf("the URL was scheduled for a render at %s before anything answered", due)
	}
	// The host is what a run targets, so scheduling the service means
	// scheduling the host it sits on.
	if full := h.fullDueOf(t, "portal.acme.test"); !full.Equal(c.now) {
		t.Fatalf("the host is due at %s rather than now", full)
	}

	c.now = c.now.Add(time.Hour)
	answered := liveHost("portal.acme.test", ingest.Port{
		Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://portal.acme.test", Scheme: "https", StatusCode: 200, Title: "Portal",
		},
	})
	h.walk(t, c, ing, set, time.Hour, answered)

	due := h.renderDueOf(t, declared)
	if due == nil {
		t.Fatal("the service answered and the declared path earned nothing")
	}
	// And it is rendered at the path as declared. A scanned path is a
	// byproduct; a declared one is an act.
	var kind string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT kind FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, declared).Scan(&kind); err != nil {
		t.Fatalf("read the declared URL: %v", err)
	}
	if kind != "url" {
		t.Fatalf("the declared path is a %q", kind)
	}
}

func (h *harness) renderDueOf(t *testing.T, key string) *time.Time {
	t.Helper()

	var due *time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT next_fingerprint_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&due)
	if err != nil {
		t.Fatalf("read the render date of %s: %v", key, err)
	}
	return due
}
