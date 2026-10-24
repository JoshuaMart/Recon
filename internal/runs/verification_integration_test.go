//go:build integration

package runs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// world answers runs: a recon gets the fixture, a verification gets one host
// per target in the shape the test sets.
type world struct {
	mu     sync.Mutex
	recon  []byte
	host   func(name string) ingest.Host
	stages []string
}

func (w *world) Run(_ context.Context, req runs.Request) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if req.Domain != "" {
		return w.recon, nil
	}
	w.stages = append(w.stages, req.Stages)
	rep := ingest.Report{SchemaVersion: "1.1", Run: ingest.RunMeta{Input: "targets", Completed: true, Version: "1.2.3"}}
	for _, t := range req.Targets {
		h := w.host(t)
		if req.Stages == "resolve" {
			h.Scan, h.Ports = nil, nil
		}
		rep.Hosts = append(rep.Hosts, h)
	}
	return json.Marshal(rep)
}

type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *manualClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func live() ingest.Host {
	return ingest.Host{
		Host: "db.example.com", Status: "live", Addresses: []string{"10.1.2.3"},
		Scan: &ingest.Scan{Scanned: 32, Open: 2, Refused: 30},
		Ports: []ingest.Port{
			{Port: 5432, Protocol: "tcp", State: "open"},
			{Port: 8080, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{Scheme: "http", StatusCode: 200, Title: "Admin"}},
		},
	}
}

// verifyFixture runs the recon once, then hands over to verification with only
// db.example.com in scope of the passes.
func verifyFixture(t *testing.T, w *world) (*fixture, *manualClock) {
	t.Helper()
	ctx := context.Background()
	f := setup(t, readFixture(t))
	w.recon = readFixture(t)
	c := &manualClock{t: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	f.sched = &runs.Scheduler{
		Store: f.st, Runner: w, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Slots: 1, Timeout: 30 * time.Minute, Ports: []int{5432, 8080}, Tick: time.Hour, Now: c.now,
		Verification: true, Batch: 100, ArchiveAfter: 30 * 24 * time.Hour, Rand: func() float64 { return 0.5 },
	}
	f.recon(t)
	// Every other host off the schedule: the test follows one.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE asset_current SET next_resolve_at = NULL, next_full_at = NULL WHERE key <> 'db.example.com'`); err != nil {
		t.Fatal(err)
	}
	return f, c
}

// pass moves the clock to the host's next due date, runs the verification it
// triggers, and returns the lifecycle of the host and of its HTTP service on
// 8080. The 5432 port speaks no HTTP: that service stays a candidate.
func pass(t *testing.T, f *fixture, c *manualClock) (host, svc string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	cur := current(t, f.st, f.program.ID, "fqdn", "db.example.com")
	due := cur.NextFullAt
	if cur.NextResolveAt != nil && (due == nil || cur.NextResolveAt.Before(*due)) {
		due = cur.NextResolveAt
	}
	if due == nil {
		t.Fatal("db.example.com has no due date")
	}
	c.set(due.Add(time.Second))
	if err := f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, f)
	return current(t, f.st, f.program.ID, "fqdn", "db.example.com").Lifecycle,
		serviceState(t, f), *due
}

func serviceState(t *testing.T, f *fixture) string {
	t.Helper()
	var s string
	err := f.st.Pool.QueryRow(context.Background(),
		`SELECT lifecycle FROM asset_current WHERE key = 'db.example.com:8080/tcp'`).Scan(&s)
	if err != nil {
		return "none"
	}
	return s
}

func waitIdle(t *testing.T, f *fixture) {
	t.Helper()
	for range 600 {
		n, err := f.st.Q.CountRunning(context.Background(), "serve")
		if err != nil {
			t.Fatal(err)
		}
		var queued int
		_ = f.st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM run WHERE state = 'queued'").Scan(&queued)
		if n == 0 && queued == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("runs did not settle")
}

func TestTakenDownGoesFlappingThenInactive(t *testing.T) {
	w := &world{host: func(string) ingest.Host { return live() }}
	f, c := verifyFixture(t, w)

	// Healthy passes first, until a full has seen the 8080 service.
	for range 4 {
		if host, svc, _ := pass(t, f, c); host != "active" {
			t.Fatalf("healthy db = %s", host)
		} else if svc == "active" {
			break
		}
	}
	if svc := serviceState(t, f); svc != "active" {
		t.Fatalf("8080 = %s after healthy passes", svc)
	}

	// The host is taken down: the name stays, every port refuses.
	w.mu.Lock()
	w.host = func(string) ingest.Host {
		h := live()
		h.Scan, h.Ports = &ingest.Scan{Scanned: 32, Refused: 32}, nil
		return h
	}
	w.mu.Unlock()

	// Resolve passes still answer, so the host stays active until a full sees
	// the refusals; from then on it may only flap, then die.
	var states []string
	var firstFail, inactiveAt time.Time
	for range 16 {
		host, svc, at := pass(t, f, c)
		states = append(states, host+"/"+svc)
		switch {
		case host == "inactive":
			inactiveAt = at
		case host == "flapping" && firstFail.IsZero():
			firstFail = at
		case host == "active" && !firstFail.IsZero():
			t.Fatalf("a host still down went back to active: %v", states)
		}
		if !inactiveAt.IsZero() {
			break
		}
	}
	if inactiveAt.IsZero() {
		t.Fatalf("never inactive: %v", states)
	}
	if inactiveAt.Sub(firstFail) < 24*time.Hour {
		t.Errorf("inactive %s after the first failure: the 24 h floor did not hold (%v)", inactiveAt.Sub(firstFail), states)
	}
	if got := states[len(states)-1]; got != "inactive/inactive" {
		t.Errorf("the service whose port is refused must die with its host, got %s", got)
	}
	// Once failing, the tcp layer is re-measured on full.
	if last := w.stages[len(w.stages)-1]; last != "full" {
		t.Errorf("a failing tcp layer is re-measured on full, saw %v", w.stages)
	}

	// It comes back: one success.
	w.mu.Lock()
	w.host = func(string) ingest.Host { return live() }
	w.mu.Unlock()
	if host, svc, _ := pass(t, f, c); host != "active" || svc != "active" {
		t.Errorf("back up: host %s, service %s", host, svc)
	}
}

func TestTimeoutsNeverKill(t *testing.T) {
	w := &world{host: func(string) ingest.Host { return live() }}
	f, c := verifyFixture(t, w)
	pass(t, f, c)

	// A firewall starts dropping: nothing is refused, nothing answers.
	w.mu.Lock()
	w.host = func(string) ingest.Host {
		h := live()
		h.Scan, h.Ports = &ingest.Scan{Scanned: 32, Filtered: 32}, nil
		return h
	}
	w.mu.Unlock()
	for i := range 12 {
		host, svc, _ := pass(t, f, c)
		if host == "inactive" || host == "flapping" || svc == "inactive" || svc == "flapping" {
			t.Fatalf("pass %d: host %s, service %s: a timeout is not informative", i, host, svc)
		}
	}
}

func TestNxdomainKillsOnResolve(t *testing.T) {
	w := &world{host: func(string) ingest.Host { return live() }}
	f, c := verifyFixture(t, w)
	for range 4 {
		if _, svc, _ := pass(t, f, c); svc == "active" {
			break
		}
	}
	w.mu.Lock()
	w.host = func(name string) ingest.Host { return ingest.Host{Host: name, Status: "dead", Reason: "nxdomain"} }
	w.mu.Unlock()
	var last, svc string
	var states []string
	for range 12 {
		last, svc, _ = pass(t, f, c)
		states = append(states, last)
		if last == "inactive" {
			break
		}
	}
	if last != "inactive" {
		t.Fatalf("an nxdomain confirmed over 24 h must kill, got %v (stages %v)", states, w.stages)
	}
	if svc != "inactive" {
		t.Errorf("the services of a name that is gone must die with it, 8080 is %s", svc)
	}
	// The dns layer failing is re-measured on resolve, the cheap rung.
	resolves := 0
	for _, s := range w.stages {
		if s == "resolve" {
			resolves++
		}
	}
	if resolves < 3 {
		t.Errorf("stages = %v", w.stages)
	}
}

func TestVerificationStaysInItsTargets(t *testing.T) {
	ctx := context.Background()
	w := &world{host: func(string) ingest.Host {
		h := live()
		h.Host = "intruder.example.com"
		return h
	}}
	f, c := verifyFixture(t, w)
	pass(t, f, c)
	var n int
	if err := f.st.Pool.QueryRow(ctx, "SELECT count(*) FROM asset WHERE key = 'intruder.example.com'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a verification created a host that was not one of its targets")
	}
	runsList, _ := f.st.Q.ListRuns(ctx, sqlcgen.ListRunsParams{Lim: 1})
	var s ingest.Summary
	_ = json.Unmarshal(runsList[0].Run.Ingest, &s)
	if s.OutsidePerimeter != 1 {
		t.Errorf("summary = %+v", s)
	}
}

func TestPausingAProgramClearsItsDueDates(t *testing.T) {
	ctx := context.Background()
	w := &world{host: func(string) ingest.Host { return live() }}
	f, _ := verifyFixture(t, w)
	if _, err := f.st.Q.UpdateProgram(ctx, sqlcgen.UpdateProgramParams{ID: f.program.ID, Name: f.program.Name, State: "paused", Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.ClearProgramSchedule(ctx, f.program.ID); err != nil {
		t.Fatal(err)
	}
	if cur := current(t, f.st, f.program.ID, "fqdn", "db.example.com"); cur.NextFullAt != nil || cur.NextResolveAt != nil {
		t.Errorf("a paused program kept due dates: %v %v", cur.NextResolveAt, cur.NextFullAt)
	}
}

func dueOf(t *testing.T, f *fixture, key string) (resolve, full *time.Time) {
	t.Helper()
	cur := current(t, f.st, f.program.ID, "fqdn", key)
	return cur.NextResolveAt, cur.NextFullAt
}

// A target the run never measures must not stay due: the next tick would queue
// it again at once, forever.
func TestUnreachedTargetsDoNotLoop(t *testing.T) {
	for name, host := range map[string]func(string) ingest.Host{
		"resolver error": func(n string) ingest.Host {
			return ingest.Host{Host: n, Status: "discovered", Reason: "resolver_error"}
		},
		"deadline": func(n string) ingest.Host { return ingest.Host{Host: n, Status: "discovered"} },
		"absent":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			w := &world{host: func(string) ingest.Host { return live() }}
			f, c := verifyFixture(t, w)
			pass(t, f, c)
			w.mu.Lock()
			if host == nil {
				w.host = func(string) ingest.Host { return ingest.Host{Host: "elsewhere.example.com", Status: "live"} }
			} else {
				w.host = host
			}
			w.mu.Unlock()
			_, _, at := pass(t, f, c)
			res, full := dueOf(t, f, "db.example.com")
			for _, d := range []*time.Time{res, full} {
				if d != nil && !d.After(at.Add(time.Second)) {
					t.Fatalf("a due date stayed at %v after the pass at %v", d, at)
				}
			}
			if host := current(t, f.st, f.program.ID, "fqdn", "db.example.com"); host.Lifecycle != "active" {
				t.Errorf("an unmeasured host moved to %s", host.Lifecycle)
			}
		})
	}
}

func TestResolverErrorsMakeUnobservable(t *testing.T) {
	w := &world{host: func(string) ingest.Host { return live() }}
	f, c := verifyFixture(t, w)
	pass(t, f, c)
	w.mu.Lock()
	w.host = func(n string) ingest.Host {
		return ingest.Host{Host: n, Status: "discovered", Reason: "resolver_error"}
	}
	w.mu.Unlock()
	var last string
	for range 6 {
		if last, _, _ = pass(t, f, c); last == "unobservable" {
			break
		}
	}
	if last != "unobservable" {
		t.Errorf("SERVFAIL three times is unobservable, got %s", last)
	}
}

// Behind a CDN the sweep tries 80 and 443 only: a service on another port was
// not measured, and must not be killed for it.
func TestUnsweptPortsAreNotRefused(t *testing.T) {
	w := &world{host: func(string) ingest.Host { return live() }}
	f, c := verifyFixture(t, w)
	for range 4 {
		if _, svc, _ := pass(t, f, c); svc == "active" {
			break
		}
	}
	w.mu.Lock()
	w.host = func(string) ingest.Host {
		h := live()
		h.CDN = []ingest.CDN{{Name: "cloudflare", Type: "cdn", ScanLimited: true}}
		h.Scan = &ingest.Scan{Scanned: 2, Open: 1, Refused: 1}
		h.Ports = []ingest.Port{{Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{Scheme: "https", StatusCode: 200}}}
		return h
	}
	w.mu.Unlock()
	for range 8 {
		if _, svc, _ := pass(t, f, c); svc != "active" {
			t.Fatalf("a service on an unswept port went %s", svc)
		}
	}
}

func TestWildcardAnswersDoNotPromote(t *testing.T) {
	w := &world{host: func(n string) ingest.Host {
		return ingest.Host{Host: n, Status: "wildcard", Addresses: []string{"10.9.9.9"}}
	}}
	f, c := verifyFixture(t, w)
	ctx := context.Background()
	// old.example.com is a candidate from the recon; follow it instead.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE asset_current SET next_resolve_at = NULL, next_full_at = NULL;
		UPDATE asset_current SET next_resolve_at = '2026-10-02T00:00:00Z' WHERE key = 'old.example.com'`); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 10, 2, 0, 1, 0, 0, time.UTC))
	if err := f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, f)
	if got := current(t, f.st, f.program.ID, "fqdn", "old.example.com"); got.Lifecycle != "candidate" || *got.DnsState != "wildcard" {
		t.Errorf("a wildcard answer promoted the candidate: %s / %v", got.Lifecycle, *got.DnsState)
	}
}
