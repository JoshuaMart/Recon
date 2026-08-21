//go:build integration

package search_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/search"
)

// synthetic is the size the milestone names.
const synthetic = 1_000_000

// TestAFourClauseQueryOverAMillionAssets is the assertion that decides whether
// PostgreSQL is enough.
//
// The whole search chapter rests on one bet: with the right indexes, facets
// over the filtered result hold comfortably to a few million rows per tenant,
// so a double write to Elasticsearch on day one is a trap rather than
// foresight. That bet is either measured or it is a hope, and a hope is what
// gets discovered on the day somebody's inventory grows.
//
// The distribution matters as much as the count. Every row carrying port 443
// would let any index look fast; what is generated below is skewed the way a
// real perimeter is, a few common ports against a long tail, a handful of
// technologies against forty, and most of the inventory under one apex.
func TestAFourClauseQueryOverAMillionAssets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	start := time.Now()
	h.generate(t, synthetic)
	t.Logf("%d assets generated in %s", synthetic, time.Since(start).Round(time.Second))

	// Without this the planner is working from the statistics of an empty
	// table, and it will pick a plan that says nothing about the real one. It
	// is also what the housekeeping loop does in production, and for the same
	// reason.
	h.exec(t, `ANALYZE asset, asset_current, pivot_count`)

	// Four clauses, and the shape a console actually sends: a perimeter, a
	// port, a technology, and an exclusion.
	tree := `{"op":"and","clauses":[
		{"op":"suffix","field":"host","value":".target.test"},
		{"op":"eq","field":"port","value":443},
		{"op":"contains","field":"technologies","value":"nginx"},
		{"op":"not","clauses":[{"op":"eq","field":"is_cdn","value":true}]}
	]}`

	h.scoped(t, h.org, func(tx pgx.Tx) {
		// One warm run first. The measurement is of the query rather than of
		// the first touch of a cold cache, which is a different question and
		// one a milestone cannot state a number for.
		if _, err := search.List(ctx, tx, h.org, search.Request{
			Filter: filter(t, tree), Display: true,
		}); err != nil {
			t.Fatalf("warm: %v", err)
		}

		at := time.Now()
		page, err := search.List(ctx, tx, h.org, search.Request{
			Filter: filter(t, tree), Display: true,
		})
		took := time.Since(at)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Rows) == 0 {
			t.Fatal("the query matched nothing, so the timing measures an empty scan")
		}
		t.Logf("a four clause page over %d assets: %s", synthetic, took.Round(time.Millisecond))
		if took > 300*time.Millisecond {
			t.Errorf("the list took %s over %d assets, and the budget is 300ms", took, synthetic)
		}

		at = time.Now()
		facets, err := search.Facets(ctx, tx, h.org, filter(t, tree))
		took = time.Since(at)
		if err != nil {
			t.Fatalf("facets: %v", err)
		}
		if len(facets) == 0 {
			t.Fatal("no facet came back")
		}
		t.Logf("the facets of the same filter: %s", took.Round(time.Millisecond))
		// Facets are the real cost, and they are what usually pushes a project
		// toward a search engine. The budget here is wider than the list's
		// because the work is: this aggregates over the whole filtered result
		// rather than over one page of it.
		if took > time.Second {
			t.Errorf("the facets took %s, which is the number that decides whether this stays "+
				"in PostgreSQL", took)
		}
	})
}

// generate writes a synthetic inventory with a distribution worth measuring.
func (h *harness) generate(t *testing.T, count int) {
	t.Helper()

	// In one statement per table rather than a loop, because a million round
	// trips would measure the test harness.
	h.exec(t, fmt.Sprintf(`
		INSERT INTO asset (id, org_id, program_id, kind, key, host, discovery_source,
		                   discovery_path, scope_status, first_seen, last_seen)
		SELECT gen_random_uuid(), $1, $2, 'service',
		       h.name || ':' || h.port || '/tcp', h.name, 'enumeration',
		       '[{"step":"enumeration"}]'::jsonb, 'in_scope',
		       now() - (i %% 90) * interval '1 day', now() - (i %% 90) * interval '1 day'
		  FROM generate_series(1, %d) AS i,
		       LATERAL (SELECT
		           -- Most of an inventory sits under one apex, with a tail
		           -- under the others. A uniform spread would make the suffix
		           -- filter behave like no filter at all.
		           'h' || i || CASE WHEN i %% 10 < 7 THEN '.target.test'
		                            WHEN i %% 10 < 9 THEN '.other.test'
		                            ELSE '.third.test' END AS name,
		           -- A few common ports against a long tail.
		           CASE WHEN i %% 10 < 6 THEN 443 WHEN i %% 10 < 8 THEN 80
		                WHEN i %% 10 = 8 THEN 8080 ELSE 1000 + (i %% 4000) END AS port) AS h`,
		count), h.org, h.program)

	h.exec(t, fmt.Sprintf(`
		INSERT INTO asset_current (
		    asset_id, org_id, program_id, kind, key, scope_status, host, port, scheme,
		    lifecycle, status_code, title, technologies, attributes, asn, country,
		    is_cdn, cdn_provider, change_buckets, buckets_day, first_seen, last_seen)
		SELECT a.id, a.org_id, a.program_id, a.kind, a.key, a.scope_status, a.host,
		       split_part(split_part(a.key, ':', 2), '/', 1)::int,
		       CASE WHEN a.key LIKE '%%:443/%%' THEN 'https' ELSE 'http' END,
		       CASE WHEN r.n %% 20 = 0 THEN 'inactive' ELSE 'active' END,
		       CASE WHEN r.n %% 5 = 0 THEN 301 WHEN r.n %% 17 = 0 THEN 403 ELSE 200 END,
		       'host ' || r.n,
		       -- A handful of technologies on most rows and forty across the
		       -- inventory, which is what makes a facet over an array cost
		       -- something.
		       ARRAY['tech' || (r.n %% 40)]
		           || CASE WHEN r.n %% 3 = 0 THEN ARRAY['nginx'] ELSE ARRAY[]::text[] END
		           || CASE WHEN r.n %% 7 = 0 THEN ARRAY['react'] ELSE ARRAY[]::text[] END,
		       CASE WHEN r.n %% 11 = 0
		            THEN jsonb_build_object('favicon_hash', 'f' || (r.n %% 500))
		            ELSE '{}'::jsonb END,
		       CASE WHEN r.n %% 4 = 0 THEN 13335 WHEN r.n %% 4 = 1 THEN 16509 ELSE 15169 END,
		       CASE WHEN r.n %% 3 = 0 THEN 'FR' WHEN r.n %% 3 = 1 THEN 'US' ELSE 'DE' END,
		       r.n %% 4 = 0,
		       CASE WHEN r.n %% 4 = 0 THEN 'cloudflare' END,
		       '{0,0,0,0,0,0,0,0}'::int[], NULL,
		       a.first_seen, a.last_seen
		  FROM (SELECT id, org_id, program_id, kind, key, scope_status, host,
		               first_seen, last_seen,
		               row_number() OVER (ORDER BY id) AS n
		          FROM asset WHERE org_id = $1) AS r
		  JOIN asset a ON a.id = r.id`), h.org)
}
