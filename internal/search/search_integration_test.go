//go:build integration

// Milestone 6's read half: what the compiler produces, measured against a
// database rather than against a string.
package search_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store"
)

type harness struct {
	pool *pgxpool.Pool
	url  string
	org  uuid.UUID
	// other is the tenant nothing here may ever see.
	other   uuid.UUID
	program uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("recon"),
		tcpostgres.WithUsername("asm_owner"),
		tcpostgres.WithPassword("owner-password-for-a-container"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	migrator, err := store.NewMigrator(url, quiet)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := migrator.Run(ctx, store.Up); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = migrator.Close()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	h := &harness{pool: pool, url: url, org: uuid.New(), other: uuid.New(), program: uuid.New()}
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'tenant'), ($2, 'the other one')`, h.org, h.other)
	h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from)
	           VALUES ($1, $2, 'p', now())`, h.program, h.org)

	// Reference data, because the badge rules read it.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := store.Seed(ctx, conn); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return h
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// asset writes one row of both tables, which is what the search reads.
func (h *harness) asset(t *testing.T, org uuid.UUID, key string, columns map[string]any) uuid.UUID {
	t.Helper()

	id := uuid.New()
	program := h.program
	if org != h.org {
		program = uuid.New()
		h.exec(t, `INSERT INTO program (id, org_id, name, authorized_from) VALUES ($1, $2, 'other', now())`,
			program, org)
	}
	kind := "service"
	if !strings.Contains(key, ":") {
		kind = "fqdn"
	}
	host, _, _ := strings.Cut(key, ":")

	seen := time.Now().UTC()
	if at, ok := columns["last_seen"].(time.Time); ok {
		seen = at
	}

	h.exec(t, `INSERT INTO asset (id, org_id, program_id, kind, key, host, discovery_source,
	                              discovery_path, scope_status, first_seen, last_seen)
	           VALUES ($1, $2, $3, $4, $5, $6, 'manual', $7, 'in_scope', $8, $8)`,
		id, org, program, kind, key, host,
		[]byte(`[{"step":"typed in"}]`), seen)

	h.exec(t, `INSERT INTO asset_current (
	        asset_id, org_id, program_id, kind, key, scope_status, host, port, scheme,
	        lifecycle, status_code, title, technologies, attributes, asn, country,
	        cdn_provider, is_cdn, change_buckets, buckets_day, first_seen, last_seen)
	     VALUES ($1, $2, $3, $4, $5, 'in_scope', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
	             $16, $17, $18, $19, $20, $20)`,
		id, org, program, kind, key, host,
		columns["port"], columns["scheme"], value(columns, "lifecycle", "active"),
		columns["status_code"], columns["title"],
		value(columns, "technologies", []string{}), value(columns, "attributes", []byte(`{}`)),
		columns["asn"], columns["country"], columns["cdn_provider"], columns["is_cdn"],
		value(columns, "change_buckets", []int32{0, 0, 0, 0, 0, 0, 0, 0}),
		columns["buckets_day"], seen)

	// The counters are maintained on write by ingestion. Here the rows are
	// written directly, so the same invariant is applied directly: pivot_count
	// is a function of the counted keys and of nothing else.
	h.exec(t, `INSERT INTO pivot_count AS pc (org_id, pivot_type, pivot_value, count)
	           SELECT $1, p.pivot_type, p.pivot_value, 1
	             FROM asset_current c, pivot_values(c.attributes) p
	            WHERE c.asset_id = $2
	           ON CONFLICT (org_id, pivot_type, pivot_value) DO UPDATE SET count = pc.count + 1`,
		org, id)
	return id
}

func value(columns map[string]any, name string, fallback any) any {
	if found, ok := columns[name]; ok {
		return found
	}
	return fallback
}

// scoped runs one query the way a request does: in a transaction carrying the
// organization, so the policies apply as well as the compiler's clause.
func (h *harness) scoped(t *testing.T, org uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()

	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, org.String()); err != nil {
		t.Fatalf("scope: %v", err)
	}
	fn(tx)
}

func filter(t *testing.T, raw string) search.Node {
	t.Helper()

	node, err := search.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return node
}

// inventory is the set every case below reads.
func (h *harness) inventory(t *testing.T) {
	t.Helper()

	h.asset(t, h.org, "app.target.test:443/tcp", map[string]any{
		"port": 443, "scheme": "https", "status_code": 200, "title": "App",
		"technologies": []string{"nginx", "react"}, "asn": 13335, "country": "FR",
		"attributes": []byte(`{"favicon_hash":"shared","cookie_names":["SESS_INTERNAL","PHPSESSID"],
		                       "script_hashes":["bundle"],"external_hosts":["cdn.partner.test"]}`),
	})
	h.asset(t, h.org, "api.target.test:443/tcp", map[string]any{
		"port": 443, "scheme": "https", "status_code": 200, "title": "API",
		"technologies": []string{"nginx"}, "asn": 13335, "country": "FR",
		"attributes": []byte(`{"favicon_hash":"shared","cookie_names":["PHPSESSID"]}`),
	})
	h.asset(t, h.org, "old.target.test:80/tcp", map[string]any{
		"port": 80, "scheme": "http", "status_code": 301, "title": "Moved",
		"technologies": []string{"apache"}, "asn": 16509, "country": "US",
		"attributes": []byte(`{"favicon_hash":"alone"}`),
	})
	h.asset(t, h.org, "evil-target.test:443/tcp", map[string]any{
		"port": 443, "scheme": "https", "status_code": 200,
		"technologies": []string{"nginx"},
	})
	h.asset(t, h.org, "target.test", map[string]any{"lifecycle": "candidate"})

	// The other tenant, carrying the same shapes. Nothing below may ever see
	// one of these rows, and a filter that named them could not: the field does
	// not exist in the registry.
	h.asset(t, h.other, "app.target.test:443/tcp", map[string]any{
		"port": 443, "scheme": "https", "status_code": 200,
		"technologies": []string{"nginx"}, "asn": 13335, "country": "FR",
		"attributes": []byte(`{"favicon_hash":"shared"}`),
	})
}

func TestASuffixReadsTheInventoryUnderOneDomain(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.List(context.Background(), tx, h.org, search.Request{
			Filter: filter(t, `{"op":"suffix","field":"host","value":".target.test"}`),
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		keys := keysOf(page)
		// A string suffix and not domain membership: the dot is in the pattern,
		// so "evil-target.test" does not come back, and "target.test" itself
		// does not either.
		if strings.Contains(strings.Join(keys, ","), "evil-target") {
			t.Errorf("evil-target.test came back under .target.test: %v", keys)
		}
		for _, key := range keys {
			if key == "target.test" {
				t.Errorf("the apex came back under its own dotted suffix: %v", keys)
			}
		}
		if len(keys) != 3 {
			t.Errorf("%d assets under .target.test, want 3: %v", len(keys), keys)
		}
	})
}

// TestTheFacetsReflectTheFilteredResult is what separates a facet from a
// statistic.
//
// The side counters of an ASM interface are aggregations over the current
// filtered result, recomputed on every query. Global counts would answer a
// different question, and nobody asked it.
func TestTheFacetsReflectTheFilteredResult(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		ctx := context.Background()

		global, err := search.Facets(ctx, tx, h.org, filter(t, `{"op":"and"}`))
		if err != nil {
			t.Fatalf("facets: %v", err)
		}
		if got := term(global, "port", "443"); got != 3 {
			t.Errorf("port 443 over the whole inventory = %d, want 3", got)
		}
		if got := term(global, "country", "FR"); got != 2 {
			t.Errorf("FR over the whole inventory = %d, want 2", got)
		}

		narrowed, err := search.Facets(ctx, tx, h.org,
			filter(t, `{"op":"suffix","field":"host","value":".target.test"}`))
		if err != nil {
			t.Fatalf("facets: %v", err)
		}
		if got := term(narrowed, "port", "443"); got != 2 {
			t.Errorf("port 443 under .target.test = %d, want 2: the facet is not reading the filter", got)
		}
		if got := term(narrowed, "technologies", "nginx"); got != 2 {
			t.Errorf("nginx under .target.test = %d, want 2", got)
		}
		// A technology outside the filter must not appear at all, which is the
		// discriminating half: a global aggregation would carry it.
		if got := term(narrowed, "technologies", "apache"); got != 1 {
			t.Errorf("apache under .target.test = %d, want 1", got)
		}
	})
}

// TestOneTenantNeverSeesAnother, without any query naming an organization.
func TestOneTenantNeverSeesAnother(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.other, func(tx pgx.Tx) {
		page, err := search.List(context.Background(), tx, h.other, search.Request{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Rows) != 1 {
			t.Errorf("the other tenant reads %d rows, want its own 1", len(page.Rows))
		}
	})

	// And the counters are per organization, so a tenant cannot infer another's
	// inventory size from a number.
	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.List(context.Background(), tx, h.org, search.Request{
			Filter:  filter(t, `{"op":"eq","field":"favicon_hash","value":"shared"}`),
			Display: true,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Rows) != 2 {
			t.Fatalf("%d assets carry the shared favicon, want 2", len(page.Rows))
		}
		for _, row := range page.Rows {
			for _, pivot := range row.Pivots {
				if pivot.Type == "favicon" && pivot.Count != 2 {
					t.Errorf("the shared favicon counts %d, want 2: the counter is not per organization",
						pivot.Count)
				}
			}
		}
	})
}

// TestAPivotLeadingOnlyToItselfIsNotBadged, and neither is a universal name.
func TestAPivotLeadingOnlyToItselfIsNotBadged(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.List(context.Background(), tx, h.org, search.Request{
			Filter:  filter(t, `{"op":"suffix","field":"host","value":".target.test"}`),
			Display: true,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		for _, row := range page.Rows {
			for _, pivot := range row.Pivots {
				badged := pivot.Badge != nil && *pivot.Badge
				switch {
				case pivot.Value == "alone" && badged:
					t.Error("a favicon carried by one asset is badged, and it leads only to itself")
				case pivot.Value == "PHPSESSID" && badged:
					t.Error("PHPSESSID is badged, and it is noise because it is universal")
				case pivot.Value == "SESS_INTERNAL" && badged:
					t.Error("an application cookie on one asset is badged")
				case pivot.Value == "shared" && !badged:
					t.Error("a favicon linking two assets is not badged, and that is the case that matters")
				}
			}
		}

		// And the data is all there. The denylist removes a badge, never a
		// value: the explicit search still works.
		found, err := search.List(context.Background(), tx, h.org, search.Request{
			Filter: filter(t, `{"op":"contains","field":"cookie_name","value":"PHPSESSID"}`),
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(found.Rows) != 2 {
			t.Errorf("a search for PHPSESSID returns %d rows, want 2: the denylist removed data", len(found.Rows))
		}
	})
}

// TestTheExportAppliesNoDisplayFilterAndImposesNoCap.
func TestTheExportAppliesNoDisplayFilterAndImposesNoCap(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		ctx := context.Background()

		var out bytes.Buffer
		written, err := search.Export(ctx, tx, h.org, filter(t, `{"op":"and"}`),
			search.FormatJSONL, 0, into(&out))
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if written != 5 {
			t.Errorf("%d assets exported, want the whole tenant's 5", written)
		}

		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != written {
			t.Fatalf("%d lines for %d assets", len(lines), written)
		}
		// A file does not say what it does not contain, so a display decision
		// inside one is invisible. The export answers the question it can
		// answer, the counter, and leaves the badge unanswered.
		if strings.Contains(out.String(), `"badge"`) {
			t.Error("the export carries a badge decision, which is a display filter inside a file")
		}
		if !strings.Contains(out.String(), "PHPSESSID") {
			t.Error("the export dropped a value the denylist names, which the rule forbids")
		}
		// Lossless: attributes and lineage travel with it.
		var first map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
			t.Fatalf("decode a line: %v", err)
		}
		if _, ok := first["attributes"]; !ok {
			t.Error("the JSONL export has no attributes, and it is the lossless one")
		}
		if _, ok := first["lineage"]; !ok {
			t.Error("the JSONL export has no lineage")
		}

		// A limit can be asked for; it is never imposed.
		out.Reset()
		written, err = search.Export(ctx, tx, h.org, filter(t, `{"op":"and"}`),
			search.FormatJSONL, 2, into(&out))
		if err != nil {
			t.Fatalf("bounded export: %v", err)
		}
		if written != 2 {
			t.Errorf("a limit of 2 wrote %d rows", written)
		}

		// CSV flattens, and the loss is the one that was named.
		out.Reset()
		if _, err := search.Export(ctx, tx, h.org, filter(t, `{"op":"and"}`),
			search.FormatCSV, 0, into(&out)); err != nil {
			t.Fatalf("csv export: %v", err)
		}
		header, _, _ := strings.Cut(out.String(), "\n")
		if strings.Contains(header, "attributes") || strings.Contains(header, "lineage") {
			t.Errorf("the CSV header carries a nested object: %s", header)
		}
		if !strings.Contains(header, "volatility") || !strings.Contains(header, "technologies") {
			t.Errorf("the CSV header is missing what it is supposed to flatten: %s", header)
		}
	})
}

// TestThePaginationIsStableAcrossAWalk.
func TestThePaginationIsStableAcrossAWalk(t *testing.T) {
	h := newHarness(t)

	// Every row carrying the same instant, which is what a report writing a
	// batch in one transaction produces. Without a tiebreaker on the identity,
	// which rows come back is the planner's choice and a walk can repeat or
	// skip one.
	at := time.Now().UTC().Truncate(time.Second)
	for i := range 25 {
		h.asset(t, h.org, fmt.Sprintf("h%02d.target.test:443/tcp", i),
			map[string]any{"port": 443, "last_seen": at})
	}

	h.scoped(t, h.org, func(tx pgx.Tx) {
		ctx := context.Background()
		seen := map[string]int{}
		cursor := search.Cursor{}
		for pages := 0; pages < 20; pages++ {
			page, err := search.List(ctx, tx, h.org, search.Request{Limit: 7, Cursor: cursor})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			for _, row := range page.Rows {
				seen[row.Key]++
			}
			if page.Next == "" {
				break
			}
			var err2 error
			if cursor, err2 = search.ParseCursor(page.Next); err2 != nil {
				t.Fatalf("cursor: %v", err2)
			}
		}

		if len(seen) != 25 {
			t.Errorf("the walk saw %d distinct assets, want 25", len(seen))
		}
		for key, times := range seen {
			if times != 1 {
				t.Errorf("%s came back %d times in one walk", key, times)
			}
		}
	})
}

// TestAnExportThatFailsBeforeItBeginsIsNotAnEmptyFile.
//
// The export streams, so past the first page a failure cannot become a status
// any more. Before it, it still can, and that is the difference between a
// caller reading "the database refused" and a caller reading "my inventory is
// empty" off a zero byte file.
func TestAnExportThatFailsBeforeItBeginsIsNotAnEmptyFile(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		ctx := context.Background()

		// A statement that cannot run: the transaction is left aborted, so the
		// first page of the walk fails.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM no_such_table`); err == nil {
			t.Fatal("the poisoning statement succeeded")
		}

		var out bytes.Buffer
		opened := false
		written, err := search.Export(ctx, tx, h.org, filter(t, `{"op":"and"}`),
			search.FormatJSONL, 0, func() io.Writer {
				opened = true
				return &out
			})
		if err == nil {
			t.Fatal("an export over a broken transaction reported success")
		}
		if written != 0 {
			t.Errorf("%d rows written by a failed export", written)
		}
		if opened {
			t.Error("the export committed to a response before its first page came back, so the " +
				"caller reads a database failure as an empty inventory")
		}
		if out.Len() != 0 {
			t.Errorf("a failed export wrote %d bytes", out.Len())
		}
	})
}

// into hands the export a writer, which is what a handler does once it has
// decided the response is going to happen.
func into(w io.Writer) func() io.Writer {
	return func() io.Writer { return w }
}

func keysOf(page search.Page) []string {
	out := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		out = append(out, row.Key)
	}
	return out
}

func term(page search.FacetPage, field, value string) int {
	for _, facet := range page.Facets {
		if facet.Field != field {
			continue
		}
		for _, entry := range facet.Terms {
			if entry.Value == value {
				return entry.Count
			}
		}
	}
	return 0
}

// TestNoQueryTouchesTheJournal is the boundary demonstrated rather than read.
//
// Principle 1 does not say nobody reads the journal. It says the interface
// queries asset_current, and what stays true is the substance: the list, the
// facets and the export never touch it, because those are what run over a
// million rows and on every keystroke.
//
// Rereading the code is a statement about the reader. Revoking the privilege
// makes the database answer.
func TestNoQueryTouchesTheJournal(t *testing.T) {
	h := newHarness(t)
	h.inventory(t)
	ctx := context.Background()

	h.exec(t, `ALTER ROLE asm_app WITH LOGIN PASSWORD 'app-password-for-a-container'`)
	h.exec(t, `REVOKE SELECT ON observation FROM asm_app`)
	// The privilege is granted by ALTER DEFAULT PRIVILEGES, which does not
	// reach the partitions: revoking on the parent alone leaves every one of
	// them readable, and a test that stopped here would prove nothing.
	h.exec(t, `DO $$
	           DECLARE part record;
	           BEGIN
	               FOR part IN SELECT c.oid::regclass AS name FROM pg_class c
	                            JOIN pg_inherits i ON i.inhrelid = c.oid
	                           WHERE i.inhparent = 'observation'::regclass
	               LOOP
	                   EXECUTE format('REVOKE SELECT ON %s FROM asm_app', part.name);
	               END LOOP;
	           END $$`)

	url := h.url
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	appURL := fmt.Sprintf("postgres://asm_app:%s@%s:%d/%s?sslmode=disable",
		"app-password-for-a-container", cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer app.Close()

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, h.org.String()); err != nil {
		t.Fatalf("scope: %v", err)
	}

	// The revocation has to bite, or everything below passes on a privilege
	// that was never removed.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM observation LIMIT 1`); err == nil {
		t.Fatal("the application role still reads the journal, so this test measures nothing")
	}
	_ = tx.Rollback(ctx)

	tx, err = app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, h.org.String()); err != nil {
		t.Fatalf("scope: %v", err)
	}

	page, err := search.List(ctx, tx, h.org, search.Request{Display: true})
	if err != nil {
		t.Fatalf("the list reads the journal: %v", err)
	}
	if len(page.Rows) == 0 {
		t.Fatal("the list came back empty, so it proves nothing about the journal")
	}
	if _, err := search.Facets(ctx, tx, h.org, filter(t, `{"op":"and"}`)); err != nil {
		t.Errorf("the facets read the journal: %v", err)
	}
	var out bytes.Buffer
	if _, err := search.Export(ctx, tx, h.org, filter(t, `{"op":"and"}`),
		search.FormatJSONL, 0, into(&out)); err != nil {
		t.Errorf("the export reads the journal: %v", err)
	}
}
