//go:build integration

// Milestone 4, in the assertions an ingestion can answer.
package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
)

// found builds a discovery report over a perimeter.
func found(hosts ...ingest.Host) ingest.Report {
	return ingest.Report{
		SchemaVersion: "1.1",
		Run: ingest.RunInfo{
			Input: "domain", Domain: "acme.test", Scope: "full",
			Completed: true, Version: "1.3.0",
		},
		Sources: []ingest.Source{
			{Name: "submd", Status: "ok", Found: len(hosts)},
			{Name: "crt", Status: "ok", Found: len(hosts)},
			{Name: "chaos", Status: "skipped_no_key"},
		},
		Hosts: hosts,
	}
}

func discovered(name string, sources ...string) ingest.Host {
	return ingest.Host{
		Host: name, Status: ingest.StatusLive,
		Addresses: []string{"93.184.216.34"}, Sources: sources,
		Ports: []ingest.Port{{Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://" + name, Scheme: "https", StatusCode: 200, Title: "App",
		}}},
	}
}

func (h *harness) discovery() ingest.Run {
	run := h.run()
	run.Kind = "discovery"
	run.Scope = "enum"
	return run
}

// Assets enter through the normal path: ingestion, scope, due dates. Nothing
// about a discovery run gets its own write path, which is why a discovery and a
// verification report deduplicate against each other correctly.
func TestADiscoveryReportEntersThroughTheOrdinaryPath(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	report := found(
		discovered("www.acme.test", "submd", "crt"),
		discovered("api.acme.test", "crt"),
		// Outside the perimeter. Stored, and never probed.
		discovered("shop.example.org", "crt"),
	)

	summary, err := ing.Report(context.Background(), h.queries, h.discovery(), set, report)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// A source with no key reports itself, and the run says so in one line.
	if line := summary.Queried(); line != "2 of 3 sources answered, 1 had no key" {
		t.Fatalf("the accounting reads %q", line)
	}

	if n := h.count(t,
		`SELECT count(*) FROM asset_current
		  WHERE program_id = $1 AND scope_status = 'in_scope' AND next_resolve_at IS NOT NULL`,
		h.program); n != 2 {
		t.Fatalf("%d in-scope hosts are scheduled", n)
	}
	// No out of scope asset receives a due date.
	if n := h.count(t,
		`SELECT count(*) FROM asset_current
		  WHERE program_id = $1 AND scope_status <> 'in_scope'
		    AND (next_resolve_at IS NOT NULL OR next_full_at IS NOT NULL
		         OR next_fingerprint_at IS NOT NULL)`,
		h.program); n != 0 {
		t.Fatalf("%d assets outside the perimeter carry a due date", n)
	}

	// discovery_path is populated and usable on every asset, which is what
	// justifies a scan to whoever owns the target.
	var path []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT discovery_path FROM asset WHERE program_id = $1 AND key = 'api.acme.test'`,
		h.program).Scan(&path); err != nil {
		t.Fatalf("read the lineage: %v", err)
	}
	var steps []map[string]any
	if err := json.Unmarshal(path, &steps); err != nil {
		t.Fatalf("decode the lineage: %v", err)
	}
	if len(steps) == 0 || steps[0]["step"] != "enumerated" {
		t.Fatalf("the lineage reads %v", steps)
	}
	sources, _ := steps[0]["sources"].([]any)
	if len(sources) != 1 || sources[0] != "crt" {
		t.Fatalf("the lineage attributes %v, and per host attribution is what makes it usable", sources)
	}

	// And it is usable, which means indexed rather than merely present.
	if n := h.count(t,
		`SELECT count(*) FROM asset
		  WHERE program_id = $1 AND discovery_path @> '[{"sources": ["crt"]}]'::jsonb`,
		h.program); n != 3 {
		t.Fatalf("%d assets are findable by the source that returned them, and only the three names were", n)
	}

	// A derived service was implied by a port rather than returned by a source.
	// Attributing it to whichever source returned the name would make "what did
	// this source find" answer with every service in the inventory.
	var step map[string]any
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT discovery_path -> 0 FROM asset WHERE program_id = $1 AND key = 'api.acme.test:443/tcp'`,
		h.program).Scan(&raw); err != nil {
		t.Fatalf("read the service lineage: %v", err)
	}
	if err := json.Unmarshal(raw, &step); err != nil {
		t.Fatalf("decode the service lineage: %v", err)
	}
	if step["step"] != "derived" || step["port"] != float64(443) {
		t.Fatalf("a derived service reads %v", step)
	}
}

// The same perimeter twice is the same measurement twice, and a measurement
// that did not change is not a row. Without this the timeline is a list of
// probes rather than a list of changes.
func TestRescanningTheSamePerimeterWritesNothingNew(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	report := found(discovered("www.acme.test", "submd"), discovered("api.acme.test", "crt"))

	if _, err := ing.Report(context.Background(), h.queries, h.discovery(), set, report); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	before := h.count(t, `SELECT count(*) FROM observation WHERE org_id = $1`, h.org)

	c.now = c.now.Add(72 * time.Hour)
	second, err := ing.Report(context.Background(), h.queries, h.discovery(), set, report)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	after := h.count(t, `SELECT count(*) FROM observation WHERE org_id = $1`, h.org)
	if after != before {
		t.Fatalf("%d observations became %d on an unchanged perimeter", before, after)
	}
	if second.Deduplicated != second.Observations {
		t.Fatalf("%d of %d observations deduplicated", second.Deduplicated, second.Observations)
	}
	if second.Created != 0 {
		t.Fatalf("%d assets were created a second time", second.Created)
	}

	// And the window moved, which is what makes the row mean "this state held
	// from here to there" rather than "somebody looked once".
	var observed, confirmed time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT o.observed_at, o.last_confirmed_at FROM observation o
		   JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key = 'www.acme.test' AND o.layer = 'dns'`,
		h.program).Scan(&observed, &confirmed); err != nil {
		t.Fatalf("read the window: %v", err)
	}
	if !confirmed.After(observed) {
		t.Fatalf("the confirmation window is a point at %s", observed)
	}
}

// A run killed midway keeps everything already delivered. FastRecon returns the
// hosts it did not reach as discovered, so the gap is stated rather than
// mistaken for an absence.
func TestARunKilledMidwayKeepsWhatItDelivered(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	truncated := found(discovered("www.acme.test", "submd"))
	truncated.Run.Completed = false
	truncated.Run.TruncatedByTimeout = true
	for n := range 3 {
		truncated.Hosts = append(truncated.Hosts, ingest.Host{
			Host: fmt.Sprintf("unreached-%d.acme.test", n), Status: ingest.StatusDiscovered,
		})
	}

	summary, err := ing.Report(context.Background(), h.queries, h.discovery(), set, truncated)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if summary.Hosts != 4 || summary.Created != 4+1 {
		t.Fatalf("the report wrote %d hosts and created %d assets", summary.Hosts, summary.Created)
	}

	// The host it reached is complete: its service, its observations, its
	// schedule.
	if state := h.lifecycleOf(t, "www.acme.test"); state != lifecycle.Active {
		t.Fatalf("the delivered host is %q", state)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'service'`, h.program); n != 1 {
		t.Fatalf("%d services survived a truncated run", n)
	}

	// The ones it never reached exist, and nothing was concluded about them.
	if n := h.count(t,
		`SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id
		  WHERE a.program_id = $1 AND a.key LIKE 'unreached-%'`, h.program); n != 0 {
		t.Fatalf("%d observations about hosts the run never reached", n)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset_current
		  WHERE program_id = $1 AND key LIKE 'unreached-%' AND lifecycle = 'candidate'`,
		h.program); n != 3 {
		t.Fatalf("%d unreached hosts are candidates", n)
	}
}

// A discovery report and a verification report are the same measurement over
// different hosts, so they deduplicate against each other. A second producer on
// the same layer would produce a false change on every asset at the second pass.
func TestDiscoveryAndVerificationDoNotPoisonEachOther(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	if _, err := ing.Report(context.Background(), h.queries, h.discovery(), set,
		found(discovered("www.acme.test", "submd"))); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	before := h.count(t, `SELECT count(*) FROM observation WHERE org_id = $1`, h.org)

	c.now = c.now.Add(24 * time.Hour)
	verification := h.run()
	verification.Kind = "verification"
	verification.Scope = lifecycle.RungFull
	verification.Targets = map[string]struct{}{"www.acme.test": {}}

	// The same host, seen by the same engine over a different input. Sources
	// are absent in targets mode, which is the only difference in the payload.
	report := found(discovered("www.acme.test"))
	report.Run.Input = "targets"
	report.Sources = nil

	if _, err := ing.Report(context.Background(), h.queries, verification, set, report); err != nil {
		t.Fatalf("verification: %v", err)
	}

	if after := h.count(t, `SELECT count(*) FROM observation WHERE org_id = $1`, h.org); after != before {
		t.Fatalf("%d observations became %d when the other mandate looked at the same host", before, after)
	}
}
