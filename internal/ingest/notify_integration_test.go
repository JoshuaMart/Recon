//go:build integration

// Milestone 5, in the assertions an ingestion can answer.
package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	guid "github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/notify"
)

type event struct {
	Kind       string
	Priority   string
	Key        string
	Summary    string
	Suppressed bool
	Payload    map[string]any
}

func (h *harness) events(t *testing.T) []event {
	t.Helper()

	rows, err := h.pool.Query(context.Background(),
		`SELECT e.kind, e.priority, coalesce(a.key, ''), e.suppressed, e.payload
		   FROM notification_event e LEFT JOIN asset a ON a.id = e.asset_id
		  WHERE e.org_id = $1 ORDER BY e.id`, h.org)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	var out []event
	for rows.Next() {
		var e event
		var raw []byte
		if err := rows.Scan(&e.Kind, &e.Priority, &e.Key, &e.Suppressed, &raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		e.Summary, _ = e.Payload["summary"].(string)
		out = append(out, e)
	}
	return out
}

func (h *harness) kinds(t *testing.T, kind string) []event {
	t.Helper()

	var out []event
	for _, e := range h.events(t) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A run that has already discovered something is out of grace, which is the
// state most of these assertions need.
func (h *harness) discovered(t *testing.T) {
	t.Helper()

	exec(t, h.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, finished_at)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'completed', now(), now())`,
		h.org, h.program)
}

func (h *harness) out(c *clock) ingest.Run {
	run := h.run()
	run.Grace = notify.Grace{CompletedDiscovery: true}
	return run
}

// An application version bump produces a readable notification saying what
// changed. A hash would answer "did it change" and nothing else, and the value
// it stands for is already in the database.
func TestAVersionBumpSaysWhatMoved(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.discovered(t)

	serving := func(tech ...string) ingest.Report {
		return liveHost("app.acme.test", ingest.Port{
			Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
				URL: "https://app.acme.test", Scheme: "https", StatusCode: 200,
				Title: "App", Tech: tech,
			},
		})
	}

	run := h.out(c)
	if _, err := ing.Report(context.Background(), h.queries, run, set, serving("nginx 1.24.0", "PHP 8.2")); err != nil {
		t.Fatalf("first: %v", err)
	}
	c.now = c.now.Add(24 * time.Hour)
	if _, err := ing.Report(context.Background(), h.queries, run, set, serving("nginx 1.25.3", "PHP 8.2")); err != nil {
		t.Fatalf("second: %v", err)
	}

	changed := h.kinds(t, notify.KindTechChanged)
	if len(changed) != 1 {
		t.Fatalf("%d technology events: %+v", len(changed), h.events(t))
	}
	for _, want := range []string{"nginx 1.25.3", "nginx 1.24.0"} {
		if !contains(changed[0].Summary, want) {
			t.Errorf("the summary %q does not carry %q", changed[0].Summary, want)
		}
	}
	// Every notification carries the lineage, not just the current state.
	if _, carried := changed[0].Payload["lineage"]; !carried {
		t.Error("the event carries no lineage")
	}
}

// A rescan with no real change produces no notification. Without this the
// product is a cron job that mails you every hour.
func TestARescanWithNoChangeNotifiesNothing(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.discovered(t)

	report := liveHost("app.acme.test", ingest.Port{
		Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://app.acme.test", Scheme: "https", StatusCode: 200, Title: "App",
		},
	})

	run := h.out(c)
	if _, err := ing.Report(context.Background(), h.queries, run, set, report); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := len(h.events(t))

	c.now = c.now.Add(72 * time.Hour)
	if _, err := ing.Report(context.Background(), h.queries, run, set, report); err != nil {
		t.Fatalf("second: %v", err)
	}
	if after := len(h.events(t)); after != before {
		t.Fatalf("%d events became %d on an unchanged rescan", before, after)
	}
}

// A dangling CNAME recorded in phase 2 produces a critical notification without
// recollecting anything: the finding is in the observation that was already
// written.
func TestADanglingCNAMENotifiesCriticallyWithItsEvidence(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	h.discovered(t)

	orphan := deadHost("old.acme.test", ingest.ReasonNXDomain, "bucket.s3.example.net.")
	if _, err := h.dated(c).Report(context.Background(), h.queries, h.out(c), set, orphan); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	found := h.kinds(t, notify.KindTakeover)
	if len(found) != 1 {
		t.Fatalf("%d takeover events", len(found))
	}
	if found[0].Priority != notify.Critical {
		t.Fatalf("a takeover is %q priority", found[0].Priority)
	}
	finding, _ := found[0].Payload["finding"].(map[string]any)
	if finding["target"] != "bucket.s3.example.net" || finding["signature"] != "nxdomain" {
		t.Fatalf("the evidence reads %v", finding)
	}
}

// A fingerprinter version bump produces diffs classified as detection improved,
// with no alert. Untreated it would alert across a whole inventory after one
// update.
func TestAVersionBumpOfTheInstrumentIsNotAnAlert(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.discovered(t)
	h.answering(t, c, ing, set, 200, "App", "nginx")

	first := page(200, "App", "nginx")
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), first); err != nil {
		t.Fatalf("render: %v", err)
	}

	// The same asset, a newer instrument, strictly more detected.
	better := page(200, "App", "nginx")
	better.Version = "2.2.0"
	better.Technologies = append(better.Technologies,
		fingerprint.Technology{Name: "Grafana"}, fingerprint.Technology{Name: "Prometheus"})
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), better); err != nil {
		t.Fatalf("render: %v", err)
	}

	improved := h.kinds(t, notify.KindDetection)
	if len(improved) != 1 {
		t.Fatalf("%d detection events: %+v", len(improved), h.events(t))
	}
	if improved[0].Priority != notify.Low {
		t.Fatalf("a revelation is %q priority", improved[0].Priority)
	}
	// And it did not also fire as a change, which is the alert this classifies
	// away.
	if changed := h.kinds(t, notify.KindTechChanged); len(changed) != 0 {
		t.Fatalf("%d technology change events beside the revelation", len(changed))
	}
}

// A program event with no asset is accepted, and a takeover without one is
// refused by the database. The nullability is a rule rather than a permission.
func TestTheShapeOfAnEventIsEnforcedBelowTheCode(t *testing.T) {
	h := newHarness(t)

	exec(t, h.pool, `INSERT INTO notification_event (org_id, program_id, kind, priority, payload)
		VALUES ($1, $2, 'program_unobservable', 'high', '{}'::jsonb)`, h.org, h.program)

	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO notification_event (org_id, program_id, kind, priority, payload)
		 VALUES ($1, $2, 'takeover_candidate', 'critical', '{}'::jsonb)`, h.org, h.program)
	if err == nil {
		t.Fatal("a takeover with no asset was accepted")
	}

	id := guid.New()
	exec(t, h.pool, `INSERT INTO asset
		(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1,$2,$3,'fqdn','x.acme.test','x.acme.test','fixture','in_scope', now(), now())`,
		id, h.org, h.program)
	_, err = h.pool.Exec(context.Background(),
		`INSERT INTO notification_event (org_id, program_id, asset_id, kind, priority, payload)
		 VALUES ($1, $2, $3, 'digest', 'medium', '{}'::jsonb)`, h.org, h.program, id)
	if err == nil {
		t.Fatal("a summary claiming to designate an asset was accepted")
	}
}

// A first run produces one summary's worth of suppression rather than a flood,
// and the suppressed rows stay readable.
func TestAFirstRunIsHeldBackAndStaysReadable(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// A discovery run exists and none has completed: the grace holds.
	exec(t, h.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
		VALUES (gen_random_uuid(), $1, $2, 'discovery', 'full', 'running', now() + interval '1 hour')`,
		h.org, h.program)

	run := h.run()
	run.Grace = notify.Grace{AnyDiscovery: true, Assets: 40, CreatedAt: c.now}

	var hosts []ingest.Host
	for n := range 40 {
		hosts = append(hosts, discovered(fmt.Sprintf("h%02d.acme.test", n), "crt"))
	}
	if _, err := ing.Report(context.Background(), h.queries, run, set, found(hosts...)); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	arrivals := h.kinds(t, notify.KindNewActive)
	if len(arrivals) == 0 {
		t.Fatal("a first run produced no new_active rows at all")
	}
	for _, e := range arrivals {
		if !e.Suppressed {
			t.Fatalf("%s notified during a first run", e.Key)
		}
	}
	// Nothing is lost: the rows are there, readable and not sent.
	if n := h.count(t,
		`SELECT count(*) FROM notification_event WHERE org_id = $1 AND suppressed`, h.org); n == 0 {
		t.Fatal("the held back events are not in the database")
	}
}

// contains keeps the assertion readable without pulling strings into a file
// that is about events.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// The comparison runs on the normalized structure on both sides. Comparing the
// payload as it arrived against the stored one reports every field
// normalization touches as a change, which is the false change the whole
// approach exists to remove.
func TestTheDiffComparesNormalizedAgainstNormalized(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.discovered(t)
	h.answering(t, c, ing, set, 200, "App", "nginx")

	// Two renders whose only real difference is the title. Everything else the
	// normalizer touches, the version it drops and the cookie map it turns into
	// names, is identical and must produce nothing.
	first := page(200, "App", "nginx")
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), first); err != nil {
		t.Fatalf("render: %v", err)
	}
	second := page(200, "App v2", "nginx")
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), second); err != nil {
		t.Fatalf("render: %v", err)
	}

	// Nothing the normalizer owns is in the comparison at all, which is what
	// the revelation classification depends on: a version appearing as a
	// change makes every pure addition read as a mixed one.
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT payload FROM notification_event
		  WHERE org_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`,
		h.org, notify.KindChainChanged).Scan(&raw); err != nil {
		t.Fatalf("read the event: %v", err)
	}
	for _, forbidden := range []string{`"version"`, `"cookies"`} {
		if contains(string(raw), `"field":`+forbidden) {
			t.Errorf("a field the normalizer owns was compared: %s", raw)
		}
	}
	// The real change is still seen. On this layer the title lives inside the
	// chain, so it reads as a chain change, which is what that layer can
	// honestly say: the raw probe is the detector for a title and it carries
	// one at the top level.
	if len(h.kinds(t, notify.KindChainChanged)) != 1 {
		t.Fatalf("the real change produced %d events: %+v",
			len(h.kinds(t, notify.KindChainChanged)), h.events(t))
	}
}

// A name that stays dangling is re-derived from every report, and critical
// escapes the windows. Telling it from the transition path would re-send the
// same alert on every scan, forever, for every dangling asset in an inventory.
func TestADanglingCNAMEIsToldOnceRatherThanEveryPass(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.discovered(t)

	orphan := deadHost("old.acme.test", ingest.ReasonNXDomain, "bucket.s3.example.net.")
	for range 4 {
		if _, err := ing.Report(context.Background(), h.queries, h.out(c), set, orphan); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		c.now = c.now.Add(12 * time.Hour)
	}

	if found := h.kinds(t, notify.KindTakeover); len(found) != 1 {
		t.Fatalf("%d critical alerts for one finding confirmed four times", len(found))
	}
	// And the finding is still there, held open rather than re-announced.
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE program_id = $1 AND attributes ? 'takeover_candidate'`,
		h.program); n != 1 {
		t.Fatalf("the finding stopped being recorded: %d assets carry it", n)
	}
}
