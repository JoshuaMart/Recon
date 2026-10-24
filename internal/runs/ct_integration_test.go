//go:build integration

package runs_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/ct"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/runs"
)

func stream(t *testing.T, f *fixture, c *manualClock, perHour int) *ct.Stream {
	t.Helper()
	s := &ct.Stream{Store: f.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: c.now, MaxPerHour: perHour, Reload: time.Hour}
	if err := s.LoadApexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func certificate(names ...string) ct.Certificate {
	return ct.Certificate{Names: names, Issuer: "Let's Encrypt YE2", Log: "Google Argon2027h1", Seen: time.Now()}
}

func inLane(t *testing.T, f *fixture, key string) bool {
	t.Helper()
	var lane bool
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT ct_lane FROM asset_current WHERE key = $1`, key).Scan(&lane); err != nil {
		return false
	}
	return lane
}

func TestCertificateNamesEnterTheLane(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	c := &manualClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s := stream(t, f, c, 2)

	if err := s.Handle(ctx, certificate("staging.example.com", "*.example.com", "admin.example.com", "example.com.evil.net", "other.org")); err != nil {
		t.Fatal(err)
	}
	got := current(t, f.st, f.program.ID, "fqdn", "staging.example.com")
	if got.Lifecycle != "candidate" || !inLane(t, f, "staging.example.com") || got.NextResolveAt == nil || !got.NextResolveAt.Equal(c.now()) {
		t.Fatalf("staging = %+v, want a candidate in the lane, resolve due now", got)
	}
	// Excluded by a local rule: kept out of scope, never probed.
	if inLane(t, f, "admin.example.com") {
		t.Error("an excluded name entered the lane")
	}
	var n int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM asset WHERE key LIKE '%evil.net' OR key = 'other.org'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d names outside every wildcard were created", n)
	}
	// The same wildcard name in another log is the same certificate.
	if err := s.Handle(ctx, certificate("*.example.com")); err != nil {
		t.Fatal(err)
	}
	s.Flush(ctx)
	w, _ := f.st.Q.GetWildcard(ctx, f.wildcard)
	if w.CtWildcards != 1 {
		t.Errorf("wildcard SANs counted = %d, want 1", w.CtWildcards)
	}
	var source, step string
	_ = f.st.Pool.QueryRow(ctx, `SELECT discovery_source, discovery_path->0->>'step' FROM asset WHERE key = 'staging.example.com'`).Scan(&source, &step)
	if source != "certstream" || step != "certificate" {
		t.Errorf("lineage = %s %s", source, step)
	}

	// The ceiling: two names this hour for the program, the third counted.
	if err := s.Handle(ctx, certificate("a.example.com", "b.example.com")); err != nil {
		t.Fatal(err)
	}
	if inLane(t, f, "b.example.com") {
		t.Error("the hourly ceiling let a third name in")
	}
	c.set(c.now().Add(time.Hour))
	if err := s.Handle(ctx, certificate("b.example.com")); err != nil {
		t.Fatal(err)
	}
	if !inLane(t, f, "b.example.com") {
		t.Error("the next hour refused a name")
	}

	// Archived, a new certificate revives it; alive, it is left alone.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE asset_current SET lifecycle = 'archived', ct_lane = false, next_resolve_at = NULL WHERE key = 'a.example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE asset_current SET lifecycle = 'active', ct_lane = false, next_resolve_at = NULL WHERE key = 'b.example.com'`); err != nil {
		t.Fatal(err)
	}
	s = stream(t, f, c, 10) // a fresh cache, as after a restart
	if err := s.Handle(ctx, certificate("a.example.com", "b.example.com")); err != nil {
		t.Fatal(err)
	}
	if got := current(t, f.st, f.program.ID, "fqdn", "a.example.com"); got.Lifecycle != "candidate" || !inLane(t, f, "a.example.com") {
		t.Errorf("an archived name with a new certificate = %s, lane %v", got.Lifecycle, inLane(t, f, "a.example.com"))
	}
	if inLane(t, f, "b.example.com") {
		t.Error("an active name re-entered the lane")
	}
}

// With verification off, the lane still probes what a certificate brought:
// resolve, then full, then the notification, then the host leaves the lane.
func TestTheLaneRunsWithVerificationOff(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	w := &world{host: func(name string) ingest.Host { h := live(); h.Host = name; return h }}
	c := &manualClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	f.sched = &runs.Scheduler{
		Store: f.st, Runner: w, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Slots: 1, Timeout: 30 * time.Minute, Ports: []int{5432, 8080}, Tick: time.Hour, Now: c.now,
		Verification: false, Batch: 100, Rand: func() float64 { return 0.5 }, PublicURL: "http://recon.test",
	}
	// A host of the inventory, due for verification: off, it must stay put.
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO asset (program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1, 'fqdn', 'old.example.com', 'old.example.com', 'manual', 'in_scope', now(), now())`, f.program.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO asset_current (asset_id, program_id, kind, key, host, scope_status, lifecycle, first_seen, last_seen, lifecycle_since, next_resolve_at)
		SELECT id, program_id, kind, key, host, scope_status, 'candidate', now(), now(), now(), $1 FROM asset WHERE key = 'old.example.com'`, c.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := stream(t, f, c, 10).Handle(ctx, certificate("new.example.com")); err != nil {
		t.Fatal(err)
	}
	// A service left from an earlier life of the name: answering then, not
	// in this run, so not a name serving HTTP now.
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO asset (program_id, kind, key, host, port, parent_asset_id, discovery_source, scope_status, first_seen, last_seen)
		SELECT program_id, 'service', 'new.example.com:9443/tcp', host, 9443, id, 'fastrecon', 'in_scope', '2026-01-01', '2026-01-01' FROM asset WHERE key = 'new.example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO asset_current (asset_id, program_id, kind, key, host, port, scope_status, lifecycle, first_seen, last_seen, lifecycle_since, http_state, status_code)
		SELECT id, program_id, kind, key, host, port, scope_status, 'inactive', first_seen, last_seen, first_seen, 'responding', 200 FROM asset WHERE key = 'new.example.com:9443/tcp'`); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		cur := current(t, f.st, f.program.ID, "fqdn", "new.example.com")
		due := cur.NextResolveAt
		if cur.NextFullAt != nil {
			due = cur.NextFullAt
		}
		if due == nil {
			t.Fatalf("new.example.com has no due date: %+v", cur)
		}
		c.set(due.Add(time.Second))
		if err := f.sched.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, f)
	}

	if !slices.Equal(w.stages, []string{"resolve", "full"}) {
		t.Fatalf("stages = %v, want resolve then full", w.stages)
	}
	var verifications, candidates int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'verification'), count(*) FILTER (WHERE kind = 'candidate') FROM run`).Scan(&verifications, &candidates)
	if verifications != 0 || candidates != 2 {
		t.Fatalf("runs: %d verifications, %d candidates; want 0 and 2", verifications, candidates)
	}
	if got := current(t, f.st, f.program.ID, "fqdn", "new.example.com"); got.Lifecycle != "active" || inLane(t, f, "new.example.com") {
		t.Fatalf("after its full: %s, lane %v; want active, out of the lane", got.Lifecycle, inLane(t, f, "new.example.com"))
	}
	ns, err := f.st.Q.RecentNotifications(ctx)
	if err != nil || len(ns) != 1 || ns[0].Kind != "ct_live" || ns[0].Title != "Example" || ns[0].Total != 1 ||
		!slices.Contains(ns[0].Items, "new.example.com:8080 · 200 · Admin") {
		t.Fatalf("notifications = %+v %v", ns, err)
	}
}
