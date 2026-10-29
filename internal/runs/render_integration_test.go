//go:build integration

package runs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/render"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

type fakeScanner struct {
	mu      sync.Mutex
	result  func(url string) (*fingerprint.Result, error)
	urls    []string
	version string
}

func (f *fakeScanner) Scan(_ context.Context, url string) (*fingerprint.Result, []byte, error) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	fn := f.result
	f.mu.Unlock()
	r, err := fn(url)
	return r, nil, err
}

func (f *fakeScanner) Health(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func jpegB64(t *testing.T, shade uint8) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	for y := range 400 {
		for x := range 640 {
			c := color.RGBA{shade, shade, shade, 255}
			if y < 60 || (x > 100 && x < 400 && y > 150 && y < 300) {
				c = color.RGBA{255 - shade, 100, 30, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func page(t *testing.T, nginx string, shade uint8) *fingerprint.Result {
	shot := jpegB64(t, shade)
	title := "API"
	return &fingerprint.Result{
		Chain: []fingerprint.Hop{{
			URL: "https://api.example.com/", StatusCode: 200, Title: &title,
			Headers: map[string]string{"Server": "nginx", "Date": time.Now().String()},
		}},
		Technologies: []fingerprint.Technology{{Name: "Nginx", Version: nginx, Category: "Web servers"}, {Name: "Cloudflare", Category: "WAF"}},
		Cookies:      map[string]any{"session": "x"},
		Metadata:     &fingerprint.Metadata{RobotsTXT: true},
		Screenshot:   &shot, Version: "v1.4.0",
	}
}

type renderFixture struct {
	*fixture
	scanner *fakeScanner
	disp    *render.Dispatcher
	clock   *manualClock
	store   fingerprint.Dir
}

func renderSetup(t *testing.T) *renderFixture {
	t.Helper()
	f := setup(t, readFixture(t))
	f.recon(t)
	c := &manualClock{t: time.Now().UTC().Add(time.Minute)}
	sc := &fakeScanner{version: "v1.4.0"}
	dir := fingerprint.Dir{Root: t.TempDir()}
	disp := &render.Dispatcher{
		Writer:  &render.Writer{Store: f.st, Storage: dir, Settings: render.DefaultSettings, Now: c.now, Rand: func() float64 { return 0.5 }},
		Scanner: sc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Max: 4, Tick: time.Hour,
	}
	return &renderFixture{fixture: f, scanner: sc, disp: disp, clock: c, store: dir}
}

// drain runs dispatcher passes until nothing is due or in flight.
func (r *renderFixture) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// The daily maintenance keeps the monthly partitions ahead of the clock; a
	// test moving its clock weeks ahead must too, or its writes have no month.
	if err := r.st.EnsurePartitions(ctx, r.clock.now()); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if err := r.disp.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if s := r.disp.Status(); s.InFlight == 0 {
			var due int
			_ = r.st.Pool.QueryRow(ctx, `SELECT count(*) FROM asset_current WHERE next_render_at <= $1`, r.clock.now()).Scan(&due)
			if due == 0 {
				return
			}
		}
	}
}

func count(t *testing.T, r *renderFixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEveryNewWebServiceGetsARender(t *testing.T) {
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.24", 240), nil }
	// The recon found two web services, api:443 and api:80; db:5432 speaks no HTTP.
	if n := count(t, r, `SELECT count(*) FROM asset_current WHERE next_render_at IS NOT NULL`); n != 2 {
		t.Fatalf("services due for a render after the recon: %d, want 2", n)
	}
	r.drain(t)
	if len(r.scanner.urls) != 2 {
		t.Fatalf("rendered %v", r.scanner.urls)
	}
	svc := current(t, r.st, r.program.ID, "service", "api.example.com:443/tcp")
	if svc.LastRenderAt == nil || svc.NextRenderAt.Before(r.clock.now().Add(20*24*time.Hour)) || svc.RenderPriority != 0 {
		t.Errorf("after a render: last %v next %v priority %d", svc.LastRenderAt, svc.NextRenderAt, svc.RenderPriority)
	}
	if svc.Waf == nil || *svc.Waf != "cloudflare" || svc.HasRobots == nil || !*svc.HasRobots {
		t.Errorf("render projection = waf %v robots %v", svc.Waf, svc.HasRobots)
	}
	if n := count(t, r, `SELECT count(*) FROM asset_tech t JOIN asset a ON a.id = t.asset_id WHERE a.key = 'api.example.com:443/tcp' AND t.name = 'Nginx' AND t.version = '{1,24}'`); n != 1 {
		t.Error("the parsed version was not stored")
	}
	if n := count(t, r, `SELECT count(*) FROM screenshot`); n != 2 {
		t.Errorf("screenshots = %d, want one per service", n)
	}
	if n := count(t, r, `SELECT count(*) FROM change WHERE kind LIKE 'tech_%'`); n != 0 {
		t.Errorf("a first render is a first sight, not %d technology changes", n)
	}
}

func TestRerenderDeduplicatesAndRecordsTechChanges(t *testing.T) {
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.24", 240), nil }
	r.drain(t)
	obs := count(t, r, `SELECT count(*) FROM observation WHERE layer = 'fingerprint'`)

	// Same page, same screenshot: a periodic render writes nothing new.
	r.clock.set(r.clock.now().Add(22 * 24 * time.Hour))
	r.drain(t)
	if n := count(t, r, `SELECT count(*) FROM observation WHERE layer = 'fingerprint'`); n != obs {
		t.Errorf("an identical render wrote %d observations", n-obs)
	}
	if n := count(t, r, `SELECT count(*) FROM screenshot`); n != 2 {
		t.Errorf("an identical screenshot was kept: %d", n)
	}

	// A precision gain, then a real upgrade with a new look.
	r.scanner.mu.Lock()
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.24.0", 240), nil }
	r.scanner.mu.Unlock()
	r.clock.set(r.clock.now().Add(22 * 24 * time.Hour))
	r.drain(t)
	r.scanner.mu.Lock()
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.26.1", 20), nil }
	r.scanner.mu.Unlock()
	r.clock.set(r.clock.now().Add(22 * 24 * time.Hour))
	r.drain(t)

	if n := count(t, r, `SELECT count(*) FROM change WHERE kind = 'tech_version_changed' AND after->>'precision' = 'true'`); n != 2 {
		t.Errorf("precision changes = %d, want one per service", n)
	}
	if n := count(t, r, `SELECT count(*) FROM change WHERE kind = 'tech_version_changed' AND after->>'precision' = 'false'`); n != 2 {
		t.Errorf("upgrades = %d, want one per service", n)
	}
	if n := count(t, r, `SELECT count(*) FROM screenshot`); n != 4 {
		t.Errorf("a new look keeps a second screenshot per service: %d", n)
	}
}

func TestBusyRendererWritesNothing(t *testing.T) {
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) {
		return nil, &fingerprint.BusyError{RetryAfter: time.Hour}
	}
	if err := r.disp.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(t, r, `SELECT count(*) FROM observation WHERE layer = 'fingerprint'`); n != 0 {
		t.Errorf("a 429 wrote %d observations", n)
	}
	if n := count(t, r, `SELECT count(*) FROM asset_current WHERE next_render_at <= $1`, r.clock.now()); n != 2 {
		t.Errorf("refused renders must stay due: %d due", n)
	}
	if s := r.disp.Status(); s.Limit != 2 {
		t.Errorf("a 429 halves the concurrency: limit %d", s.Limit)
	}
}

func TestUnreachableTargetIsRetriedAndCountsTowardUnobservable(t *testing.T) {
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) { return nil, fingerprint.ErrUnreachable }
	r.drain(t)
	svc := current(t, r.st, r.program.ID, "service", "api.example.com:443/tcp")
	if svc.NextRenderAt == nil || svc.NextRenderAt.After(r.clock.now().Add(7*time.Hour)) {
		t.Errorf("retry at %v, want about 6 h", svc.NextRenderAt)
	}
	if n := count(t, r, `SELECT count(*) FROM observation WHERE layer = 'fingerprint' AND outcome = 'error'`); n != 2 {
		t.Errorf("unreachable observations = %d", n)
	}
	if svc.Lifecycle != "active" {
		t.Errorf("a render failure moved the service to %s", svc.Lifecycle)
	}
}

// A service that keeps refusing backs off, and a render that works ends it.
func TestTargetFailuresInARowBackOff(t *testing.T) {
	r := renderSetup(t)
	fail := true
	r.scanner.result = func(string) (*fingerprint.Result, error) {
		if fail {
			return nil, &fingerprint.UnreachableError{Reason: "connection_refused"}
		}
		return page(t, "1.24", 240), nil
	}
	svc := func() sqlcgen.AssetCurrent {
		return current(t, r.st, r.program.ID, "service", "api.example.com:443/tcp")
	}
	for i, want := range []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour} {
		r.drain(t)
		s := svc()
		if s.RenderFailures != int16(i+1) || s.NextRenderAt == nil {
			t.Fatalf("failure %d: count %d next %v", i+1, s.RenderFailures, s.NextRenderAt)
		}
		// The writer's jitter is 10 %, and the fixture's random is its midpoint.
		// Its clock is PostgreSQL's, to the microsecond.
		if got := s.NextRenderAt.Sub(r.clock.now().Truncate(time.Microsecond)); got != want {
			t.Errorf("failure %d: retried after %s, want %s", i+1, got, want)
		}
		r.clock.set(*s.NextRenderAt)
	}
	if n := count(t, r, `SELECT count(*) FROM observation o JOIN asset a ON a.id = o.asset_id
		WHERE a.key = 'api.example.com:443/tcp' AND o.layer = 'fingerprint' AND o.data->>'browser' = 'connection_refused'`); n == 0 {
		t.Error("the reason was not recorded")
	}
	fail = false
	r.drain(t)
	if s := svc(); s.RenderFailures != 0 || s.NextRenderAt.Before(r.clock.now().Add(20*24*time.Hour)) {
		t.Errorf("after a success: count %d next %v, want 0 and the periodic cadence", s.RenderFailures, s.NextRenderAt)
	}
}

// A page the renderer answered without its capture is rendered again soon; a
// redirect, which has none to give, is not.
func TestARenderWithoutItsCaptureIsRetried(t *testing.T) {
	r := renderSetup(t)
	shot := false
	r.scanner.result = func(url string) (*fingerprint.Result, error) {
		res := page(t, "1.24", 240)
		if url == "http://api.example.com/" {
			res.Chain = append(res.Chain, fingerprint.Hop{URL: url, StatusCode: 301})
			res.Screenshot = nil
		} else if !shot {
			res.Screenshot = nil
		}
		return res, nil
	}
	svc := func(key string) sqlcgen.AssetCurrent { return current(t, r.st, r.program.ID, "service", key) }
	now := func() time.Time { return r.clock.now().Truncate(time.Microsecond) }
	for i, want := range []time.Duration{6 * time.Hour, 12 * time.Hour} {
		r.drain(t)
		s := svc("api.example.com:443/tcp")
		if s.RenderFailures != int16(i+1) || s.LastRenderAt == nil || s.NextRenderAt.Sub(now()) != want {
			t.Fatalf("render %d without a capture: count %d last %v next in %s, want %s",
				i+1, s.RenderFailures, s.LastRenderAt, s.NextRenderAt.Sub(now()), want)
		}
		if i == 0 {
			if red := svc("api.example.com:80/tcp"); red.RenderFailures != 0 || red.NextRenderAt.Before(now().Add(20*24*time.Hour)) {
				t.Errorf("a redirect without a capture: count %d next %v, want the periodic cadence", red.RenderFailures, red.NextRenderAt)
			}
		}
		r.clock.set(*s.NextRenderAt)
	}
	shot = true
	r.drain(t)
	if s := svc("api.example.com:443/tcp"); s.RenderFailures != 0 || s.NextRenderAt.Before(now().Add(20*24*time.Hour)) {
		t.Errorf("with its capture: count %d next %v, want 0 and the periodic cadence", s.RenderFailures, s.NextRenderAt)
	}
}

// A page that sends the browser to another host by JavaScript, an SSO login,
// has no page of the target to capture either: it is not retried, and where it
// went is kept, without the query that changes on every render.
func TestAnExternalRedirectIsNotRetried(t *testing.T) {
	r := renderSetup(t)
	const sso = "https://login.example.net/authorize?client=api&state=x1"
	r.scanner.result = func(url string) (*fingerprint.Result, error) {
		res := page(t, "1.24", 240)
		res.Chain[0].URL = url
		res.Screenshot = nil
		res.ExternalRedirect = sso
		return res, nil
	}
	r.drain(t)
	now := r.clock.now().Truncate(time.Microsecond)
	for _, key := range []string{"api.example.com:443/tcp", "api.example.com:80/tcp"} {
		if s := current(t, r.st, r.program.ID, "service", key); s.RenderFailures != 0 || s.NextRenderAt.Before(now.Add(20*24*time.Hour)) {
			t.Errorf("%s: count %d next %v, want 0 and the periodic cadence", key, s.RenderFailures, s.NextRenderAt)
		}
	}
	if n := count(t, r, `SELECT count(*) FROM observation WHERE layer = 'fingerprint' AND data->>'external_redirect' = 'https://login.example.net/authorize'`); n != 2 {
		t.Errorf("the destination is in %d observations, want 2", n)
	}
}

func TestMajorVersionMakesEverythingDue(t *testing.T) {
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.24", 240), nil }
	r.drain(t)
	setVersion := func(v string) int {
		r.scanner.mu.Lock()
		r.scanner.version = v
		before := len(r.scanner.urls)
		r.scanner.mu.Unlock()
		// Past the health interval, so the next pass reads the version.
		r.clock.set(r.clock.now().Add(2 * time.Minute))
		r.drain(t)
		r.scanner.mu.Lock()
		defer r.scanner.mu.Unlock()
		return len(r.scanner.urls) - before
	}
	if n := setVersion("v1.9.0"); n != 0 {
		t.Errorf("a minor bump re-rendered %d", n)
	}
	if n := setVersion("v2.0.0"); n != 2 {
		t.Errorf("a major bump re-rendered %d, want both services", n)
	}
	// A commit hash says nothing about what changed.
	if n := setVersion("b2d2e49"); n != 0 {
		t.Errorf("a non-semver version re-rendered %d", n)
	}
}

// A queue head held back by politeness must not idle the other slots.
func TestPolitenessDoesNotStarveOtherTargets(t *testing.T) {
	ctx := context.Background()
	r := renderSetup(t)
	block := make(chan struct{})
	defer close(block)
	var mu sync.Mutex
	var seen []string
	r.scanner.result = func(url string) (*fingerprint.Result, error) {
		mu.Lock()
		seen = append(seen, url)
		mu.Unlock()
		<-block
		return nil, fingerprint.ErrUnreachable
	}
	// 300 web services on one domain, due first, and one elsewhere, due last.
	if _, err := r.st.Pool.Exec(ctx, `
		WITH s AS (
		  INSERT INTO asset (program_id, kind, key, host, port, scheme, discovery_source, scope_status, first_seen, last_seen)
		  SELECT $1, 'service', 'h'||g||'.example.com:443/tcp', 'h'||g||'.example.com', 443, 'https', 'manual', 'in_scope', now(), now()
		  FROM generate_series(1, 300) g RETURNING id, key, host)
		INSERT INTO asset_current (asset_id, program_id, kind, key, host, port, scheme, scope_status, lifecycle, http_state,
		                           first_seen, last_seen, lifecycle_since, next_render_at, render_priority)
		SELECT id, $1, 'service', key, host, 443, 'https', 'in_scope', 'active', 'responding', now(), now(), now(), $2, 1 FROM s`,
		r.program.ID, r.clock.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Pool.Exec(ctx, `UPDATE asset_current SET next_render_at = NULL WHERE key LIKE 'api.%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Pool.Exec(ctx, `
		WITH s AS (
		  INSERT INTO asset (program_id, kind, key, host, port, scheme, discovery_source, scope_status, first_seen, last_seen)
		  VALUES ($1, 'service', 'www.other.org:443/tcp', 'www.other.org', 443, 'https', 'manual', 'in_scope', now(), now()) RETURNING id)
		INSERT INTO asset_current (asset_id, program_id, kind, key, host, port, scheme, scope_status, lifecycle, http_state,
		                           first_seen, last_seen, lifecycle_since, next_render_at, render_priority)
		SELECT id, $1, 'service', 'www.other.org:443/tcp', 'www.other.org', 443, 'https', 'in_scope', 'active', 'responding',
		       now(), now(), now(), $2, 1 FROM s`, r.program.ID, r.clock.now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	r.disp.Max = 8
	if err := r.disp.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	other := false
	for _, u := range seen {
		other = other || u == "https://www.other.org/"
	}
	// 8 slots: 4 for the domain at the head, the politeness bound, and the
	// target 300 rows down the queue reached past it.
	if len(seen) != 5 || !other {
		t.Errorf("saw %v", seen)
	}
}

func TestGarbageCollectionKeepsWhatARowNames(t *testing.T) {
	ctx := context.Background()
	r := renderSetup(t)
	r.scanner.result = func(string) (*fingerprint.Result, error) { return page(t, "1.24", 240), nil }
	r.drain(t)
	if err := r.store.Put(ctx, "deadbeef.jpg", []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	if n, err := render.CollectGarbage(ctx, r.st.Q, r.store, time.Now()); err != nil || n != 0 {
		t.Fatalf("within the grace period nothing goes: %d, %v", n, err)
	}
	n, err := render.CollectGarbage(ctx, r.st.Q, r.store, time.Now().Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("collected %d, %v: want the orphan only", n, err)
	}
	if _, err := r.store.Get(ctx, "deadbeef.jpg"); err == nil {
		t.Error("the orphan is still there")
	}
	var key string
	if err := r.st.Pool.QueryRow(ctx, `SELECT key FROM screenshot LIMIT 1`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Get(ctx, key); err != nil {
		t.Errorf("a named screenshot was collected: %v", err)
	}
}

func TestRenderDatesFollowTheProgram(t *testing.T) {
	ctx := context.Background()
	r := renderSetup(t)
	if _, err := r.st.Q.UpdateProgram(ctx, sqlcgen.UpdateProgramParams{ID: r.program.ID, Name: r.program.Name, State: "paused", Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := inventory.SyncSchedule(ctx, r.st.Q, r.program.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, r, `SELECT count(*) FROM asset_current WHERE next_render_at IS NOT NULL`); n != 0 {
		t.Errorf("a paused program kept %d render dates", n)
	}
	if _, err := r.st.Q.UpdateProgram(ctx, sqlcgen.UpdateProgramParams{ID: r.program.ID, Name: r.program.Name, State: "active", Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := inventory.SyncSchedule(ctx, r.st.Q, r.program.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, r, `SELECT count(*) FROM asset_current WHERE next_render_at IS NOT NULL AND render_priority = 0`); n != 2 {
		t.Errorf("an active program's web services get render dates back, low priority: %d", n)
	}
}
