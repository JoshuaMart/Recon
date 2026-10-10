//go:build integration

package search_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/search"
)

// synthetic is the size the milestone names.
const synthetic = 1_000_000

// The budgets the search bet rests on, and the machine they mean something on. A wall
// clock number is about hardware as much as SQL: these were measured on the reference
// machine with room to spare, and a shared CI runner lands at 2.5 times each, saying
// nothing about the query. A budget enforced there is red for reasons nobody can act
// on, and such a suite stops being read.
//
// So the number is enforced where it means something, and the plan everywhere.
const (
	listBudget   = 300 * time.Millisecond
	facetsBudget = time.Second

	// The budgets off the reference machine: not a softer assertion but a ceiling for
	// something absurd, since what survives a change of machine is the plan.
	elsewhere = 10
)

// reference reports whether this run is on the machine the numbers were measured on.
// Opt in, as no runtime check can tell: set RECON_SCALE_REFERENCE to enforce the
// milliseconds.
func reference() bool { return os.Getenv("RECON_SCALE_REFERENCE") != "" }

func budget(strict time.Duration) (time.Duration, string) {
	if reference() {
		return strict, "the budget is"
	}
	return strict * elsewhere, "this is not the reference machine, so the ceiling is"
}

// The assertion that decides whether PostgreSQL is enough: with the right indexes, facets over
// the filtered result are bet to hold to a few million rows per tenant, making a day-one
// Elasticsearch double write a trap.
//
// "With the right indexes" is what a clock cannot check and a plan can, identically on every
// machine. Measured: dropping the six indexes this filter uses makes both timings go down (116
// and 426 ms), as a parallel sequential scan over a warm million rows beats the bitmap path, so
// the strict budget passes with every index gone. The data is skewed like a real perimeter (a few
// common ports and a long tail, a handful of technologies against forty, most under one apex),
// since uniform data would let any index look fast.
func TestAFourClauseQueryOverAMillionAssets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	start := time.Now()
	h.generate(t, synthetic)
	t.Logf("%d assets generated in %s", synthetic, time.Since(start).Round(time.Second))

	// Without this the planner uses the statistics of an empty table and picks a plan
	// that says nothing about the real one. The housekeeping loop does it in production,
	// for the same reason.
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
		// One warm run first: the measurement is the query, not the first touch of a cold
		// cache.
		if _, err := search.List(ctx, tx, h.org, search.Request{
			Filter: filter(t, tree), Display: true,
		}); err != nil {
			t.Fatalf("warm: %v", err)
		}

		listed := &recorder{inner: tx}
		at := time.Now()
		page, err := search.List(ctx, listed, h.org, search.Request{
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
		if limit, why := budget(listBudget); took > limit {
			t.Errorf("the list took %s over %d assets, and %s %s", took, synthetic, why, limit)
		}

		// The half that means the same on every machine. Which index is deliberately not asserted: the
		// planner declines the obvious asset_current_recent_idx walk for a bitmap of the reversed host
		// index against the technology one, faster because four clauses cut a million rows to a set a
		// sort barely notices, and naming an index would fail on a legitimately improved query. What
		// does not move: the tenant's slice is reached through an index, not by reading the table.
		plan := explain(t, tx, listed.on(t, "asset_current"))
		refuseSeqScan(t, plan, "asset_current", "the list")
		requireIndex(t, plan, "asset_current", "the list")

		faceted := &recorder{inner: tx}
		at = time.Now()
		facets, err := search.Facets(ctx, faceted, h.org, filter(t, tree))
		took = time.Since(at)
		if err != nil {
			t.Fatalf("facets: %v", err)
		}
		if len(facets.Facets) == 0 {
			t.Fatal("no facet came back")
		}
		t.Logf("the facets of the same filter: %s", took.Round(time.Millisecond))
		// Facets are the real cost and what usually pushes a project toward a search engine.
		// The budget is wider than the list's because the work is: it aggregates the whole
		// filtered result, not one page.
		if limit, why := budget(facetsBudget); took > limit {
			t.Errorf("the facets took %s, %s %s, and that is the number that decides whether "+
				"this stays in PostgreSQL", took, why, limit)
		}

		// The same pair on the aggregating statement. Facets have no early exit, so a scan is
		// a far more tempting plan here.
		facetPlan := explain(t, tx, faceted.on(t, "asset_current"))
		refuseSeqScan(t, facetPlan, "asset_current", "the facets")
		requireIndex(t, facetPlan, "asset_current", "the facets")
	})
}

// recorder is a Querier that keeps what it forwards, so the statement the package
// built can go to EXPLAIN. The search package exposes no SQL, and should not: asking
// the plan of a query the test wrote is a test of its own copy.
type recorder struct {
	inner search.Querier
	seen  []statement
}

type statement struct {
	sql  string
	args []any
}

func (r *recorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	// Copied, because the caller owns the slice and reuses it.
	kept := make([]any, len(args))
	copy(kept, args)
	r.seen = append(r.seen, statement{sql: sql, args: kept})
	return r.inner.Query(ctx, sql, args...)
}

// on returns the single statement that reads the named relation. Both calls also issue
// a favicon lookup by hash, not the thing measured, so it is named instead of assumed
// second.
func (r *recorder) on(t *testing.T, relation string) statement {
	t.Helper()

	var found []statement
	for _, s := range r.seen {
		if strings.Contains(s.sql, relation) {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no statement touched %s, out of the %d that ran", relation, len(r.seen))
	default:
		t.Fatalf("%d statements touched %s, so this test no longer knows which one it is "+
			"asserting about", len(found), relation)
	}
	return statement{}
}

// explain asks the planner what it intends, in the transaction that runs the real
// thing: row-level security is part of the statement, so a plan outside the tenant's
// scope is for a different query.
func explain(t *testing.T, tx pgx.Tx, s statement) map[string]any {
	t.Helper()

	rows, err := tx.Query(context.Background(), "EXPLAIN (FORMAT JSON) "+s.sql, s.args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var raw []byte
	if !rows.Next() {
		t.Fatal("explain returned nothing")
	}
	if err := rows.Scan(&raw); err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	rows.Close()

	var wrapper []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	if len(wrapper) == 0 {
		t.Fatal("the plan came back empty")
	}
	return wrapper[0].Plan
}

// walk visits every node of a plan tree, the top one included.
func walk(node map[string]any, visit func(map[string]any)) {
	if node == nil {
		return
	}
	visit(node)
	children, ok := node["Plans"].([]any)
	if !ok {
		return
	}
	for _, child := range children {
		if next, ok := child.(map[string]any); ok {
			walk(next, visit)
		}
	}
}

// requireIndex is the other half: not only that no scan happened but that an index
// reached the rows, since a plan can reach a relation through a materialized
// intermediate and show neither.
func requireIndex(t *testing.T, plan map[string]any, relation, what string) {
	t.Helper()

	var used []string
	walk(plan, func(node map[string]any) {
		if name, ok := node["Index Name"].(string); ok && strings.HasPrefix(name, relation+"_") {
			used = append(used, name)
		}
	})
	if len(used) == 0 {
		t.Errorf("%s reaches %s through no index at all, and the whole bet of this chapter is "+
			"that the right indexes exist:\n%s", what, relation, render(plan))
	}
}

// refuseSeqScan is the assertion that survives a change of machine: reading a million
// rows to answer about one tenant's slice is the regression the indexes prevent, and a
// timing on a shared runner cannot tell it from a busy afternoon.
func refuseSeqScan(t *testing.T, plan map[string]any, relation, what string) {
	t.Helper()

	walk(plan, func(node map[string]any) {
		if node["Relation Name"] != relation {
			return
		}
		kind, _ := node["Node Type"].(string)
		if strings.Contains(kind, "Seq Scan") {
			t.Errorf("%s reads %s with a %s, so the tenant's slice is being found by reading "+
				"every row:\n%s", what, relation, kind, render(plan))
		}
	})
}

// render prints the plan as a failure needs it: node types and what each touches, not
// the cost-estimate JSON nobody reads from a log.
func render(plan map[string]any) string {
	var out strings.Builder
	var depth int
	var line func(map[string]any)
	line = func(node map[string]any) {
		fmt.Fprintf(&out, "%s%v", strings.Repeat("  ", depth), node["Node Type"])
		if name, ok := node["Relation Name"]; ok {
			fmt.Fprintf(&out, " on %v", name)
		}
		if name, ok := node["Index Name"]; ok {
			fmt.Fprintf(&out, " using %v", name)
		}
		out.WriteString("\n")

		children, _ := node["Plans"].([]any)
		depth++
		for _, child := range children {
			if next, ok := child.(map[string]any); ok {
				line(next)
			}
		}
		depth--
	}
	line(plan)
	return out.String()
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
