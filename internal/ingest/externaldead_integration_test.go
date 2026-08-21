//go:build integration

// The internal half of external_host_dead, in both directions.
//
// Covering one direction covers about half the cases, and which half depends on
// the order two unrelated events happened in, which is not a property anybody
// can predict or explain.
package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
)

// deadExternal reads the events the two directions produce.
func (h *harness) deadExternal(t *testing.T) []string {
	t.Helper()

	rows, err := h.pool.Query(context.Background(),
		`SELECT payload ->> 'external_host' FROM notification_event
		  WHERE kind = 'external_host_dead' ORDER BY id`)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, host)
	}
	return out
}

// TestAPageStartingToPointAtADeadHostIsTold is the direction the referencing
// side sees.
func TestAPageStartingToPointAtADeadHostIsTold(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// The internal host dies first, through the ordinary path.
	gone := deadHost("static.acme.test", ingest.ReasonNXDomain)
	h.walk(t, c, ing, set, 12*time.Hour, gone, gone, gone)
	if state := h.lifecycleOf(t, "static.acme.test"); state != lifecycle.Inactive {
		t.Fatalf("the internal host is %q rather than dead", state)
	}

	// And only then does a render report a page loading from it.
	h.answering(t, c, ing, set, 200, "App", "nginx")
	page := rendered()
	page.ExternalHosts = []string{"static.acme.test", "cdn.partner.test"}
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page); err != nil {
		t.Fatalf("render: %v", err)
	}

	found := h.deadExternal(t)
	if len(found) != 1 || found[0] != "static.acme.test" {
		t.Fatalf("the render produced %v, want the dead internal host alone", found)
	}

	// A render that reports the same page again says nothing. It rides the
	// insert path for the same reason the takeover finding does: a reference
	// that stays is re-derived from every pass, and critical escapes the
	// windows, so telling it from anywhere else would re-alert forever.
	c.now = c.now.Add(time.Hour)
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if again := h.deadExternal(t); len(again) != 1 {
		t.Errorf("an unchanged render re-sent the finding: %v", again)
	}
}

// TestAHostDyingUnderAPageThatPointsAtItIsTold is the direction only the
// transition can see.
//
// Nothing in a payload comparison says that somebody else's page points at this
// name, and the render that recorded the reference happened before the death.
func TestAHostDyingUnderAPageThatPointsAtItIsTold(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// The reference is recorded while the host is alive, so the other direction
	// has nothing to see.
	h.answering(t, c, ing, set, 200, "App", "nginx")
	page := rendered()
	page.ExternalHosts = []string{"static.acme.test"}
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if found := h.deadExternal(t); len(found) != 0 {
		t.Fatalf("a live host produced %v", found)
	}

	// And then it dies.
	gone := deadHost("static.acme.test", ingest.ReasonNXDomain)
	h.walk(t, c, ing, set, 12*time.Hour, gone, gone, gone)
	if state := h.lifecycleOf(t, "static.acme.test"); state != lifecycle.Inactive {
		t.Fatalf("the internal host is %q rather than dead", state)
	}

	found := h.deadExternal(t)
	if len(found) != 1 || found[0] != "static.acme.test" {
		t.Fatalf("the death produced %v, want one finding naming the host", found)
	}

	// The event names what still points at it, which is the whole of what makes
	// it actionable.
	var referrer string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT payload -> 'referenced_by' ->> 0 FROM notification_event
		  WHERE kind = 'external_host_dead' LIMIT 1`).Scan(&referrer); err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	if referrer != service {
		t.Errorf("the finding names %q as the referrer, want %q", referrer, service)
	}

	// The transition happens once, so the finding is told once however many
	// further passes confirm the death.
	c.now = c.now.Add(12 * time.Hour)
	h.walk(t, c, ing, set, 12*time.Hour, gone, gone)
	if again := h.deadExternal(t); len(again) != 1 {
		t.Errorf("further confirmations of the same death produced %v", again)
	}
}

// A service going quiet is a port closing, which the lifecycle already speaks
// for. Reading it as a dead external host would raise a critical alert every
// time somebody turned off a port on a name that still resolves.
func TestAServiceGoingQuietIsNotADeadExternalHost(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// The name is alive and one of its services is not, which is the ordinary
	// state of any host that ever closed a port.
	live := uuid.New()
	dead := uuid.New()
	for _, row := range []struct {
		id        uuid.UUID
		kind, key string
		lifecycle string
	}{
		{live, "fqdn", "static.acme.test", "active"},
		{dead, "service", "static.acme.test:8080/tcp", "inactive"},
	} {
		exec(t, h.pool, `INSERT INTO asset (id, org_id, program_id, kind, key, host,
		                                    discovery_source, scope_status, first_seen, last_seen)
		                 VALUES ($1,$2,$3,$4,$5,'static.acme.test','manual','in_scope',now(),now())`,
			row.id, h.org, h.program, row.kind, row.key)
		exec(t, h.pool, `INSERT INTO asset_current (asset_id, org_id, program_id, kind, key, host,
		                                            scope_status, lifecycle, first_seen, last_seen)
		                 VALUES ($1,$2,$3,$4,$5,'static.acme.test','in_scope',$6,now(),now())`,
			row.id, h.org, h.program, row.kind, row.key, row.lifecycle)
	}

	h.answering(t, c, ing, set, 200, "App", "nginx")
	page := rendered()
	page.ExternalHosts = []string{"static.acme.test"}
	if _, err := ing.Render(context.Background(), h.queries, h.target(t), page); err != nil {
		t.Fatalf("render: %v", err)
	}

	if found := h.deadExternal(t); len(found) != 0 {
		t.Errorf("a closed port on a live name was read as a dead external host: %v", found)
	}
}
