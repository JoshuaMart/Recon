//go:build integration

package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/JoshuaMart/recon/internal/store/storetest"
)

type fakeRunner struct {
	mu       sync.Mutex
	reports  [][]byte
	err      error
	requests []runs.Request
}

func (f *fakeRunner) Run(_ context.Context, req runs.Request) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	r := f.reports[0]
	if len(f.reports) > 1 {
		f.reports = f.reports[1:]
	}
	return r, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

type fixture struct {
	st       *store.Store
	program  sqlcgen.Program
	wildcard uuid.UUID
	sched    *runs.Scheduler
	runner   *fakeRunner
}

func setup(t *testing.T, reports ...[]byte) *fixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	p, err := st.Q.CreateManualProgram(ctx, sqlcgen.CreateManualProgramParams{Name: "Example", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []scope.Rule{
		{Kind: scope.Include, Matcher: scope.Apex, Pattern: "example.com"},
		{Kind: scope.Exclude, Matcher: scope.FQDN, Pattern: "admin.example.com"},
		{Kind: scope.Exclude, Matcher: scope.Apex, Pattern: "dev.example.com"},
	} {
		if _, err := scope.OpenRule(ctx, st.Q, p.ID, r, "local", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := st.Q.ListWildcards(ctx, sqlcgen.ListWildcardsParams{DefaultAutoScan: "off", ProgramID: &p.ID})
	if err != nil || len(ws) != 1 {
		t.Fatalf("wildcards = %v, %v", ws, err)
	}
	f := &fakeRunner{reports: reports}
	c := &clock{t: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	return &fixture{
		st: st, program: p, wildcard: ws[0].ID, runner: f,
		sched: &runs.Scheduler{
			Store: st, Runner: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Slots: 1, Timeout: 30 * time.Minute, Ports: []int{80, 443}, Tick: time.Hour, Now: c.now,
		},
	}
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../ingest/testdata/recon.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recon queues a recon, lets the scheduler run it, and returns the finished run.
func (f *fixture) recon(t *testing.T) sqlcgen.Run {
	t.Helper()
	ctx := context.Background()
	r, queued, reason, err := runs.EnqueueRecon(ctx, f.st.Q, f.wildcard, "user", time.Now().UTC())
	if err != nil || !queued {
		t.Fatalf("enqueue: %v %s", err, reason)
	}
	if err := f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, err := f.st.Q.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "queued" && got.State != "running" {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return sqlcgen.Run{}
}

func summary(t *testing.T, r sqlcgen.Run) ingest.Summary {
	t.Helper()
	var s ingest.Summary
	if err := json.Unmarshal(r.Ingest, &s); err != nil {
		t.Fatalf("ingest summary %q: %v", r.Ingest, err)
	}
	return s
}

func current(t *testing.T, st *store.Store, programID uuid.UUID, kind, key string) sqlcgen.AssetCurrent {
	t.Helper()
	assets, err := st.Q.ListProgramAssets(context.Background(), programID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assets {
		if a.Kind == kind && a.Key == key {
			c, err := st.Q.GetAssetCurrent(context.Background(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	t.Fatalf("no %s asset %q", kind, key)
	return sqlcgen.AssetCurrent{}
}

func TestReconFillsTheInventory(t *testing.T) {
	ctx := context.Background()
	f := setup(t, readFixture(t))
	r := f.recon(t)
	if r.State != "succeeded" {
		t.Fatalf("run = %s: %s", r.State, *r.Error)
	}

	req := f.runner.requests[0]
	if req.Domain != "example.com" || req.Stages != "full" || req.Ports != "80,443" {
		t.Errorf("request = %+v", req)
	}
	if strings.Join(req.Exclude, " ") != "*.dev.example.com admin.example.com dev.example.com" {
		t.Errorf("exclusions = %v", req.Exclude)
	}

	s := summary(t, r)
	if s.OutsidePerimeter != 1 || s.NotReached != 1 || s.ServicesCreated != 3 {
		t.Errorf("summary = %+v", s)
	}
	// fqdn: api, db, old, mx, slow; ip: 3 addresses; services: api:443, api:80, db:5432
	if s.AssetsCreated != 5 {
		t.Errorf("hosts created = %d, want 5", s.AssetsCreated)
	}

	api := current(t, f.st, f.program.ID, "fqdn", "api.example.com")
	if api.Lifecycle != "active" || api.ScopeStatus != "in_scope" || *api.DnsState != "resolving" || *api.Cdn != "cloudflare" {
		t.Errorf("api = %+v", api)
	}
	if got := current(t, f.st, f.program.ID, "fqdn", "old.example.com"); got.Lifecycle != "candidate" || *got.DnsState != "nxdomain" {
		t.Errorf("an nxdomain from a recon is a candidate, never dead: %+v", got)
	}
	if got := current(t, f.st, f.program.ID, "fqdn", "mx.example.com"); *got.DnsState != "nodata" {
		t.Errorf("no_answer is nodata: %+v", got)
	}
	svc := current(t, f.st, f.program.ID, "service", "api.example.com:443/tcp")
	if svc.Lifecycle != "active" || *svc.StatusCode != 200 || *svc.FinalUrl != "https://api.example.com/v2" {
		t.Errorf("api:443 = %+v", svc)
	}
	if got := current(t, f.st, f.program.ID, "service", "db.example.com:5432/tcp"); got.Lifecycle != "candidate" {
		t.Errorf("a port with no HTTP answer is a candidate service: %+v", got)
	}

	w, err := f.st.Q.GetWildcard(ctx, f.wildcard)
	if err != nil || w.LastReconAt == nil || w.AssetsFound != 5 {
		t.Errorf("wildcard = %+v, %v", w, err)
	}

	// You started it: one message, with the counts and the run's changes.
	ns, err := f.st.Q.RecentNotifications(ctx)
	if err != nil || len(ns) != 1 || ns[0].Kind != "recon_finished" ||
		ns[0].Title != "*.example.com finished: "+strconv.Itoa(s.Hosts)+" hosts, 5 new, 3 new services" ||
		ns[0].Link == nil || !strings.HasSuffix(*ns[0].Link, "/changes?run="+r.ID.String()) {
		t.Errorf("notifications = %+v, %v", ns, err)
	}
}

func TestSecondReconIsDeduplicated(t *testing.T) {
	fx := readFixture(t)
	f := setup(t, fx)
	first := summary(t, f.recon(t))
	second := summary(t, f.recon(t))
	if first.ObsWritten == 0 || first.ObsWritten != first.ObsSubmitted {
		t.Errorf("first run = %+v", first)
	}
	if second.ObsSubmitted != first.ObsSubmitted || second.ObsWritten != 0 || second.DedupRate != 1 || second.Changes != 0 {
		t.Errorf("an identical report must only confirm: %+v", second)
	}
}

func TestVolatileFieldsDoNotDefeatDedup(t *testing.T) {
	fx := readFixture(t)
	// Same target, different request: response time, content length, the
	// order of addresses and SANs, the version of the scanner.
	noisy := strings.NewReplacer(
		`"response_time_ms": 251`, `"response_time_ms": 999`,
		`"content_length": 1533`, `"content_length": 1600`,
		`["93.184.216.35", "93.184.216.34"]`, `["93.184.216.34", "93.184.216.35"]`,
		`["www.example.com", "api.example.com"]`, `["api.example.com", "www.example.com"]`,
		`"version": "1.2.3"`, `"version": "1.3.0"`,
	).Replace(string(fx))
	if noisy == string(fx) {
		t.Fatal("the replacements did not apply")
	}
	f := setup(t, fx, []byte(noisy))
	f.recon(t)
	if s := summary(t, f.recon(t)); s.ObsWritten != 0 {
		t.Errorf("volatile fields wrote %d observations: %+v", s.ObsWritten, s)
	}
}

func TestChangesAreRecorded(t *testing.T) {
	ctx := context.Background()
	fx := readFixture(t)
	changed := strings.NewReplacer(
		// db opens 6379 and closes nothing; api's title changes.
		`"scan": {"scanned": 32, "open": 1, "refused": 31, "filtered": 0, "unknown": 0},
     "ports": [{"port": 5432,`,
		`"scan": {"scanned": 32, "open": 2, "refused": 30, "filtered": 0, "unknown": 0},
     "ports": [{"port": 6379, "protocol": "tcp", "state": "open", "addresses": ["10.1.2.3"]}, {"port": 5432,`,
		`"title": "API"`, `"title": "API v3"`,
		// old comes back to life.
		`{"host": "old.example.com", "status": "dead", "reason": "nxdomain", "sources": ["crt"]}`,
		`{"host": "old.example.com", "status": "live", "addresses": ["93.184.216.40"], "sources": ["crt"]}`,
	).Replace(string(fx))
	f := setup(t, fx, []byte(changed))
	f.recon(t)
	r := f.recon(t)

	kinds := map[string]int{}
	rows, err := f.st.Pool.Query(ctx, "SELECT kind FROM change WHERE run_id = $1", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds[k]++
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if kinds["port_opened"] != 1 || kinds["http_changed"] != 1 || kinds["lifecycle_changed"] != 1 || kinds["dns_changed"] != 1 {
		t.Errorf("changes = %v", kinds)
	}
	if kinds["new_asset"] != 2 { // the 6379 service and old's address
		t.Errorf("new assets = %d, want 2: %v", kinds["new_asset"], kinds)
	}
}

func TestDegradedRunCannotConcludeAbsence(t *testing.T) {
	ctx := context.Background()
	degraded := strings.Replace(string(readFixture(t)), `"degraded": []`, `"degraded": ["resolvers_unvalidated"]`, 1)
	f := setup(t, []byte(degraded))
	f.recon(t)
	var outcome string
	if err := f.st.Pool.QueryRow(ctx, `SELECT o.outcome FROM observation o JOIN asset a ON a.id = o.asset_id
		WHERE a.key = 'old.example.com' AND o.layer = 'dns'`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "error" {
		t.Errorf("an nxdomain from a degraded run is %q, want error", outcome)
	}
}

func TestBusyRunnerRequeues(t *testing.T) {
	ctx := context.Background()
	f := setup(t, readFixture(t))
	f.runner.err = runs.ErrBusy
	r, _, _, err := runs.EnqueueRecon(ctx, f.st.Q, f.wildcard, "user", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sched.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	got, _ := f.st.Q.GetRun(ctx, r.ID)
	if got.State != "queued" {
		t.Errorf("a busy runner leaves the run %s, want queued", got.State)
	}
	if _, queued, reason, _ := runs.EnqueueRecon(ctx, f.st.Q, f.wildcard, "user", time.Now().UTC()); queued || reason == "" {
		t.Error("a second recon on the same wildcard must not be queued")
	}
}

func TestFailedRunLeavesTheWildcard(t *testing.T) {
	ctx := context.Background()
	f := setup(t, readFixture(t))
	f.runner.err = errors.New("boom")
	if r := f.recon(t); r.State != "failed" || r.Error == nil {
		t.Fatalf("run = %s %v", r.State, r.Error)
	}
	w, _ := f.st.Q.GetWildcard(ctx, f.wildcard)
	if w.LastReconAt != nil {
		t.Error("a failed recon must not move last_recon_at")
	}
	ns, err := f.st.Q.RecentNotifications(ctx)
	if err != nil || len(ns) != 1 || !strings.HasPrefix(ns[0].Title, "*.example.com failed: ") {
		t.Errorf("a failed recon is announced too: %+v, %v", ns, err)
	}
}

func TestRefreshDeclaresAndReclassifies(t *testing.T) {
	ctx := context.Background()
	f := setup(t, readFixture(t))
	f.recon(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Excluding api takes the host, its services and nothing else out.
	if _, err := scope.OpenRule(ctx, f.st.Q, f.program.ID, scope.Rule{Kind: scope.Exclude, Matcher: scope.FQDN, Pattern: "api.example.com"}, "local", nil, now); err != nil {
		t.Fatal(err)
	}
	// An include fqdn under another domain declares the host.
	if _, err := scope.OpenRule(ctx, f.st.Q, f.program.ID, scope.Rule{Kind: scope.Include, Matcher: scope.FQDN, Pattern: "shop.other.com"}, "local", nil, now); err != nil {
		t.Fatal(err)
	}
	res, err := inventory.Refresh(ctx, f.st.Q, f.program, "off", now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Declared != 1 || res.Reclassified != 3 {
		t.Errorf("refresh = %+v, want 1 declared and api + its 2 services reclassified", res)
	}
	if got := current(t, f.st, f.program.ID, "service", "api.example.com:443/tcp"); got.ScopeStatus != "out_of_scope" {
		t.Errorf("api:443 = %s", got.ScopeStatus)
	}
	if got := current(t, f.st, f.program.ID, "fqdn", "shop.other.com"); got.ScopeStatus != "in_scope" || got.Lifecycle != "candidate" {
		t.Errorf("declared host = %+v", got)
	}
}

func TestAutoScanQueuesANewWildcard(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	onNew := "on_new"
	p, err := f.st.Q.UpdateProgram(ctx, sqlcgen.UpdateProgramParams{ID: f.program.ID, Name: f.program.Name, State: "active", AutoScan: &onNew, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.OpenRule(ctx, f.st.Q, p.ID, scope.Rule{Kind: scope.Include, Matcher: scope.Apex, Pattern: "new.io"}, "local", nil, now); err != nil {
		t.Fatal(err)
	}
	res, err := inventory.Refresh(ctx, f.st.Q, p, "off", now)
	if err != nil || res.AutoScans != 1 {
		t.Fatalf("refresh = %+v, %v", res, err)
	}
	runsList, _ := f.st.Q.ListRuns(ctx, sqlcgen.ListRunsParams{ProgramID: &p.ID, Lim: 10})
	if len(runsList) != 1 || runsList[0].Run.Trigger != "auto_scan" || *runsList[0].Apex != "new.io" {
		t.Errorf("runs = %+v", runsList)
	}
}

// A recon cut by a restart ends like any other: failed, and announced.
func TestRecoverAnnouncesAnInterruptedRecon(t *testing.T) {
	ctx := context.Background()
	f := setup(t, readFixture(t))
	r, queued, reason, err := runs.EnqueueRecon(ctx, f.st.Q, f.wildcard, "user", time.Now().UTC())
	if err != nil || !queued {
		t.Fatalf("enqueue: %v %s", err, reason)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE run SET state = 'running', started_at = now() WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetRun(ctx, r.ID)
	ns, err := f.st.Q.RecentNotifications(ctx)
	if got.State != "failed" || err != nil || len(ns) != 1 || ns[0].Title != "*.example.com failed: interrupted by a restart" {
		t.Fatalf("run %s, notifications %+v %v", got.State, ns, err)
	}
}
