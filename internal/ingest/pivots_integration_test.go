//go:build integration

// Milestone 6's projection half. A facet aggregates over what the table holds,
// and a counter maintained on write cannot be maintained if the write never
// sees the value, so everything else in the search chapter rests on this.
package ingest_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// rendered is a page carrying every value the projection is supposed to lift.
func rendered() *fingerprint.Result {
	result := page(200, "App", "nginx")
	result.Chain = []fingerprint.Hop{
		{URL: "http://app.acme.test/", StatusCode: 301},
		{URL: "https://app.acme.test/", StatusCode: 307},
		{URL: "https://app.acme.test/login", StatusCode: 200, Title: "App"},
	}
	favicon := "abcdef0123456789"
	result.Metadata = fingerprint.Metadata{FaviconHash: &favicon}
	result.Technologies = []fingerprint.Technology{
		{Name: "react", Version: "18.2", Category: "js"},
		{Name: "nginx", Category: "web-server"},
	}
	result.Cookies = map[string]string{"SESS_INTERNAL": "x", "PHPSESSID": "y"}
	result.ExternalHosts = []string{"cdn.partner.test"}
	result.Scripts = []fingerprint.Script{
		{URL: "https://app.acme.test/app.js", Internal: true, Hash: "bundle-hash"},
		// Served from a public CDN, so it groups thousands of unrelated sites
		// without discriminating between any of them.
		{URL: "https://cdn.example.net/jquery.js", Internal: false, Hash: "jquery-hash"},
	}
	return result
}

// withCertificate brings the service into existence through the ordinary path,
// presenting a certificate.
//
// The probe measures the certificate and the render does not, and the reason is
// coverage: the probe sees every HTTPS service on every full pass, while a
// render happens on five triggers that can be three weeks apart.
func (h *harness) withCertificate(t *testing.T, c *clock, ing *ingest.Ingestor, set *scope.Set) {
	t.Helper()

	report := liveHost("app.acme.test", ingest.Port{
		Port: 443, Protocol: "tcp", State: "open", HTTP: &ingest.HTTP{
			URL: "https://app.acme.test", Scheme: "https", StatusCode: 200,
			// One name the render will also report and one it will not, so
			// that the union is the discriminating case rather than an
			// intersection that happens to look the same.
			Title: "App", Server: "nginx", Tech: []string{"nginx", "openssl"},
			TLS: &ingest.TLS{
				SubjectCN: "app.acme.test", Issuer: "Test CA",
				CertSPKIHash: "spki-of-the-service",
			},
		},
	})
	h.walk(t, c, ing, set, 12*time.Hour, report)
}

// attributes reads the projection's JSON object.
func (h *harness) attributes(t *testing.T, key string) map[string]any {
	t.Helper()

	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT attributes FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&raw); err != nil {
		t.Fatalf("read the attributes of %s: %v", key, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the attributes of %s: %v", key, err)
	}
	return out
}

func (h *harness) pivot(t *testing.T, kind, value string) int {
	t.Helper()

	var count int
	err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(count), 0) FROM pivot_count
		  WHERE org_id = $1 AND pivot_type = $2 AND pivot_value = $3`,
		h.org, kind, value).Scan(&count)
	if err != nil {
		t.Fatalf("read the counter of %s/%s: %v", kind, value, err)
	}
	return count
}

func TestTheProjectionLiftsThePivotsOutOfThePayload(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.withCertificate(t, c, ing, set)

	if _, err := ing.Render(context.Background(), h.queries, h.target(t), rendered()); err != nil {
		t.Fatalf("render: %v", err)
	}

	attrs := h.attributes(t, service)

	if got := attrs["favicon_hash"]; got != "abcdef0123456789" {
		t.Errorf("favicon_hash = %v, want the render's hash", got)
	}

	// The internal flag is not a reading detail. A bundle from a public CDN is
	// shared by thousands of unrelated sites: it groups without discriminating,
	// which is the test that decides what is a pivot at all.
	scripts := textsOf(t, attrs["script_hashes"])
	if len(scripts) != 1 || scripts[0] != "bundle-hash" {
		t.Errorf("script_hashes = %v, want the internal one alone", scripts)
	}

	// The pivot belongs to the fingerprinter, which also sees the cookies a
	// script sets. PHPSESSID is present because the denylist removes a badge and
	// never a piece of data.
	cookies := textsOf(t, attrs["cookie_names"])
	if strings.Join(cookies, ",") != "PHPSESSID,SESS_INTERNAL" {
		t.Errorf("cookie_names = %v, want both names indexed", cookies)
	}

	if hosts := textsOf(t, attrs["external_hosts"]); len(hosts) != 1 || hosts[0] != "cdn.partner.test" {
		t.Errorf("external_hosts = %v", hosts)
	}

	// The probe's certificate, from the other layer, still in place after a
	// render rewrote its own keys. One producer per value means one layer's
	// write is not the other's erasure.
	if attrs["cert_spki_hash"] != "spki-of-the-service" {
		t.Errorf("cert_spki_hash = %v: the render erased what the probe projected", attrs["cert_spki_hash"])
	}

	// The column is the union of the two producers, so a filter on nginx
	// answers whether the probe or the browser saw it.
	var technologies []string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT technologies FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&technologies); err != nil {
		t.Fatalf("read technologies: %v", err)
	}
	if strings.Join(technologies, ",") != "nginx,openssl,react" {
		t.Errorf("technologies = %v, want the union: openssl is the probe's alone and react the "+
			"render's alone, and a filter has to answer for both", technologies)
	}

	// The version travels beside the name rather than inside it: the column is
	// queried by exact element, so "react 18.2" would not answer a filter on
	// "react".
	var versions []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	encoded, _ := json.Marshal(attrs["tech_render"])
	if err := json.Unmarshal(encoded, &versions); err != nil {
		t.Fatalf("decode tech_render: %v", err)
	}
	if len(versions) != 2 || versions[1].Name != "react" || versions[1].Version != "18.2" {
		t.Errorf("tech_render = %+v, want the render's objects with their versions", versions)
	}

	// Showing 200 is true without being the information: this page was a 301,
	// then a 307, then a 200 somewhere else.
	var chain []int32
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status_chain FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, service).Scan(&chain); err != nil {
		t.Fatalf("read status_chain: %v", err)
	}
	if len(chain) != 3 || chain[0] != 301 || chain[1] != 307 || chain[2] != 200 {
		t.Errorf("status_chain = %v, want one code per hop in order", chain)
	}
}

// TestALayerThatStopsReportingAValueStopsCountingIt is the COALESCE trap
// wearing another name.
//
// A merge alone would keep the favicon of a page that no longer has one, which
// is exactly the coalesced title that survives its own page forever, except the
// consequence here is a counter that keeps asserting.
func TestALayerThatStopsReportingAValueStopsCountingIt(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")
	target := h.target(t)

	if _, err := ing.Render(context.Background(), h.queries, target, rendered()); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if n := h.pivot(t, "favicon", "abcdef0123456789"); n != 1 {
		t.Fatalf("the favicon counter is %d after one render, want 1", n)
	}
	if n := h.pivot(t, "script", "bundle-hash"); n != 1 {
		t.Fatalf("the script counter is %d after one render, want 1", n)
	}

	// The same page, redesigned: another favicon, and the bundle gone.
	c.now = c.now.Add(2 * time.Hour)
	changed := rendered()
	other := "fedcba9876543210"
	changed.Metadata.FaviconHash = &other
	changed.Scripts = nil
	if _, err := ing.Render(context.Background(), h.queries, target, changed); err != nil {
		t.Fatalf("second render: %v", err)
	}

	if n := h.pivot(t, "favicon", "abcdef0123456789"); n != 0 {
		t.Errorf("the old favicon still counts %d assets, and it links none", n)
	}
	if n := h.pivot(t, "favicon", "fedcba9876543210"); n != 1 {
		t.Errorf("the new favicon counts %d, want 1", n)
	}
	if n := h.pivot(t, "script", "bundle-hash"); n != 0 {
		t.Errorf("a bundle the page no longer loads still counts %d", n)
	}

	attrs := h.attributes(t, service)
	if attrs["script_hashes"] != nil {
		t.Errorf("script_hashes survived a render that reported none: %v", attrs["script_hashes"])
	}
}

// TestArchivingGivesBackEveryPivotAndTheKeysWithThem is the path that gets
// forgotten.
//
// An archived asset gives back all its pivots although no value changed.
// Nothing in a payload comparison signals it: the lifecycle transition is what
// says so, which is why it travels in the statement that reschedules rather
// than in a sweep of its own.
//
// The transition is driven through that statement directly, and that is
// deliberate. Archiving is reachable today only from a candidate host that
// exhausted its budget without ever coming alive, and such a host has no
// service, no render and therefore no pivot. Waiting for a curve to produce a
// case the model cannot produce would test nothing; the statement is the thing
// under test, and it is the one the system uses.
func TestArchivingGivesBackEveryPivotAndTheKeysWithThem(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.withCertificate(t, c, ing, set)
	target := h.target(t)

	if _, err := ing.Render(context.Background(), h.queries, target, rendered()); err != nil {
		t.Fatalf("render: %v", err)
	}
	held := []struct{ kind, value string }{
		{"favicon", "abcdef0123456789"},
		{"script", "bundle-hash"},
		{"cookie_name", "SESS_INTERNAL"},
		{"cert_spki", "spki-of-the-service"},
	}
	for _, pivot := range held {
		if n := h.pivot(t, pivot.kind, pivot.value); n != 1 {
			t.Fatalf("%s/%s counts %d before archiving, want 1", pivot.kind, pivot.value, n)
		}
	}

	if err := h.queries.RescheduleAsset(context.Background(), sqlcgen.RescheduleAssetParams{
		AssetID:     pgtype.UUID{Bytes: target.AssetID, Valid: true},
		Archive:     true,
		BackoffTier: 0,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	for _, pivot := range held {
		if n := h.pivot(t, pivot.kind, pivot.value); n != 0 {
			t.Errorf("%s/%s still counts %d on an archived asset, and a pivot is a lead to follow",
				pivot.kind, pivot.value, n)
		}
	}

	// The keys go with the counters. Without that, pivot_count stops being a
	// function of asset_current and there is nowhere left to read the truth,
	// which is what makes a drifted counter unrepairable rather than merely
	// wrong.
	attrs := h.attributes(t, service)
	for _, key := range []string{"favicon_hash", "script_hashes", "cookie_names", "cert_spki_hash"} {
		if attrs[key] != nil {
			t.Errorf("%s survived archiving, so the counter and the table now disagree", key)
		}
	}
	// These carry no counter, so the invariant says nothing about them and
	// removing them would lose a filter for no reason.
	if attrs["external_hosts"] == nil {
		t.Error("external_hosts was removed by archiving, and it carries no counter")
	}
	if attrs["tech_render"] == nil {
		t.Error("tech_render was removed by archiving, and it carries no counter")
	}
}

// TestTheCounterIsAFunctionOfTheTableAndOfNothingElse is the invariant that
// makes the rest checkable.
//
// A counter that reflects nothing cannot be repaired, for lack of anywhere to
// read the truth. This one recomputes entirely from a scan of the table, and
// this is that scan.
func TestTheCounterIsAFunctionOfTheTableAndOfNothingElse(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")
	target := h.target(t)

	if _, err := ing.Render(context.Background(), h.queries, target, rendered()); err != nil {
		t.Fatalf("first render: %v", err)
	}
	c.now = c.now.Add(time.Hour)
	changed := rendered()
	changed.Cookies = map[string]string{"OTHER_NAME": "z"}
	if _, err := ing.Render(context.Background(), h.queries, target, changed); err != nil {
		t.Fatalf("second render: %v", err)
	}

	var drifted int
	err := h.pool.QueryRow(context.Background(), `
		WITH recomputed AS (
		    SELECT c.org_id, p.pivot_type, p.pivot_value, count(*)::int AS count
		      FROM asset_current c, pivot_values(c.attributes) p
		     GROUP BY c.org_id, p.pivot_type, p.pivot_value
		)
		SELECT count(*)
		  FROM pivot_count k
		  FULL OUTER JOIN recomputed r
		    ON r.org_id = k.org_id AND r.pivot_type = k.pivot_type AND r.pivot_value = k.pivot_value
		 WHERE COALESCE(k.count, 0) <> COALESCE(r.count, 0)`).Scan(&drifted)
	if err != nil {
		t.Fatalf("recount: %v", err)
	}
	if drifted != 0 {
		t.Errorf("%d counters disagree with a scan of the table", drifted)
	}
}

// A favicon is by far the fastest identity signal to read in an inventory, and
// one copy per distinct image is what makes the shared case cheap.
func TestTheFaviconImageIsStoredOncePerDistinctValueAndBounded(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)
	h.answering(t, c, ing, set, 200, "App", "nginx")
	target := h.target(t)

	small := rendered()
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("a tiny png"))
	small.Metadata.Favicon = &inline
	if _, err := ing.Render(context.Background(), h.queries, target, small); err != nil {
		t.Fatalf("render: %v", err)
	}

	var stored int
	var media string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(max(media_type), '') FROM favicon_image WHERE org_id = $1`,
		h.org).Scan(&stored, &media); err != nil {
		t.Fatalf("read favicon_image: %v", err)
	}
	if stored != 1 || media != "image/png" {
		t.Fatalf("%d images stored with media type %q, want one png", stored, media)
	}

	// The same icon again is a no-op, which is what makes writing cost nothing
	// in steady state.
	c.now = c.now.Add(time.Hour)
	again := rendered()
	again.Metadata.Favicon = &inline
	again.Cookies = map[string]string{"MOVED": "1"}
	if _, err := ing.Render(context.Background(), h.queries, target, again); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM favicon_image WHERE org_id = $1`, h.org).Scan(&stored); err != nil {
		t.Fatalf("read favicon_image: %v", err)
	}
	if stored != 1 {
		t.Errorf("%d rows for one distinct favicon, want 1", stored)
	}

	// The value is chosen by the target, and nothing stops a server serving
	// five megabytes under that name. Past the bound the hash and its counter
	// keep working and only the thumbnail is missing.
	c.now = c.now.Add(time.Hour)
	huge := rendered()
	other := "0000ffff0000ffff"
	huge.Metadata.FaviconHash = &other
	oversized := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 200<<10))
	huge.Metadata.Favicon = &oversized
	if _, err := ing.Render(context.Background(), h.queries, target, huge); err != nil {
		t.Fatalf("third render: %v", err)
	}

	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM favicon_image WHERE org_id = $1 AND hash = $2`,
		h.org, other).Scan(&stored); err != nil {
		t.Fatalf("read favicon_image: %v", err)
	}
	if stored != 0 {
		t.Error("an oversized favicon was stored, and the bound exists for a storage bill")
	}
	if n := h.pivot(t, "favicon", other); n != 1 {
		t.Errorf("the hash of an oversized favicon counts %d, want 1: only the thumbnail degrades", n)
	}
}

// TestVolatilityCountsChangesAndNotArrivals is the assertion the filter rests
// on.
//
// Incrementing on any non deduplicated observation counts the arrival of an
// asset, which the row already carries as its age, and it counts it once per
// layer, so a freshly discovered asset scores three or four. Volatility is also
// a filter, so "volatility > 2" would then return everything just discovered,
// which is the opposite of the question it asks.
func TestVolatilityCountsChangesAndNotArrivals(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: today()}
	ing := h.dated(c)

	h.answering(t, c, ing, set, 200, "App", "nginx")

	if n := h.volatility(t, service); n != 0 {
		t.Errorf("a service just discovered scores %d, and its arrival is its age rather than a change", n)
	}
	// The array itself, not only the number derived from it. The guard sits on
	// two columns, and a sum read against a day nobody wrote is zero whatever
	// the buckets hold: asserting the sum alone lets half the guard be removed
	// without a test noticing.
	if buckets := h.buckets(t, service); buckets != "{0,0,0,0,0,0,0,0}" {
		t.Errorf("a service just discovered carries %s, and an arrival is not a change", buckets)
	}
	if h.changedAt(t, service) != nil {
		t.Error("last_changed_at is set on an asset that has never changed, and the console reads that as \"never\"")
	}

	// The same page again. A confirmation of the same state is a probe, not a
	// change, and a counter that moved on every probe would score an untouched
	// asset by how often it is looked at.
	c.now = c.now.Add(time.Hour)
	h.answering(t, c, ing, set, 200, "App", "nginx")
	if n := h.volatility(t, service); n != 0 {
		t.Errorf("a rescan that changed nothing scores %d", n)
	}
	if buckets := h.buckets(t, service); buckets != "{0,0,0,0,0,0,0,0}" {
		t.Errorf("a rescan that changed nothing wrote %s", buckets)
	}

	// Now something actually moves.
	c.now = c.now.Add(time.Hour)
	h.answering(t, c, ing, set, 200, "Sign in", "nginx")
	if n := h.volatility(t, service); n != 1 {
		t.Errorf("one change scores %d, want 1", n)
	}
	if h.changedAt(t, service) == nil {
		t.Error("last_changed_at is still null after a change")
	}
}

// TestTheWindowSlidesOnTheDayTheArrayWasLastShifted is the read side of the
// trap.
//
// The array of an asset unchanged for five days has not been shifted, because
// nothing rewrote it. Summing its first seven buckets naively would count
// changes twelve days old as if they were yesterday's, and no test that does
// not move the clock across a day boundary can see it.
func TestTheWindowSlidesOnTheDayTheArrayWasLastShifted(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: today()}
	ing := h.dated(c)

	h.answering(t, c, ing, set, 200, "App", "nginx")
	c.now = c.now.Add(time.Hour)
	h.answering(t, c, ing, set, 200, "Sign in", "nginx")
	if n := h.volatility(t, service); n != 1 {
		t.Fatalf("one change scores %d before the clock moves", n)
	}

	// Nothing rewrites the row. The array still reads {1,0,...} and the day it
	// belongs to is what makes it worth zero now.
	exec(t, h.pool, `UPDATE asset_current SET buckets_day = buckets_day - 9
	                  WHERE program_id = $1 AND key = $2`, h.program, service)
	if n := h.volatility(t, service); n != 0 {
		t.Errorf("a change nine days old still scores %d: the sum is being read in the wrong frame", n)
	}

	// And a change six days old is still inside the window.
	exec(t, h.pool, `UPDATE asset_current SET buckets_day = (now() AT TIME ZONE 'UTC')::date - 6
	                  WHERE program_id = $1 AND key = $2`, h.program, service)
	if n := h.volatility(t, service); n != 1 {
		t.Errorf("a change six days old scores %d, want 1", n)
	}

	// The eighth bucket is margin: it absorbs the partial current day so the
	// seventh is not truncated, and it is deliberately not summed.
	exec(t, h.pool, `UPDATE asset_current
	                    SET change_buckets = '{0,0,0,0,0,0,0,99}',
	                        buckets_day = (now() AT TIME ZONE 'UTC')::date
	                  WHERE program_id = $1 AND key = $2`, h.program, service)
	if n := h.volatility(t, service); n != 0 {
		t.Errorf("the eighth bucket contributed %d to the window, and it is margin", n)
	}
}

// today is noon UTC on the calendar day the database is living in.
//
// The two clocks have to agree, and here that is the test's problem rather than
// the system's: the buckets are written from the observation's instant and read
// against the database's own day, which is exactly right in production and
// exactly wrong under a fixed clock set to some date in the past. Noon rather
// than the current time, so that a test adding a couple of hours does not cross
// midnight and measure the shift instead of the count.
func today() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
}

func (h *harness) volatility(t *testing.T, key string) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT volatility(change_buckets, buckets_day) FROM asset_current
		  WHERE program_id = $1 AND key = $2`, h.program, key).Scan(&n); err != nil {
		t.Fatalf("read the volatility of %s: %v", key, err)
	}
	return n
}

func (h *harness) buckets(t *testing.T, key string) string {
	t.Helper()

	var buckets string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT change_buckets::text FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&buckets); err != nil {
		t.Fatalf("read the buckets of %s: %v", key, err)
	}
	return buckets
}

func (h *harness) changedAt(t *testing.T, key string) *time.Time {
	t.Helper()

	var at *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT last_changed_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&at); err != nil {
		t.Fatalf("read last_changed_at of %s: %v", key, err)
	}
	return at
}

func textsOf(t *testing.T, value any) []string {
	t.Helper()

	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("%v is not a string", item)
		}
		out = append(out, text)
	}
	return out
}
