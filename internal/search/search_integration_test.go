//go:build integration

package search_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/storetest"
)

var now = time.Now().UTC().Truncate(time.Microsecond)

type svc struct {
	program  string
	host     string
	port     int
	status   int
	title    string
	server   string
	cdn      string
	favicon  string
	age      time.Duration
	state    string
	triage   string
	techs    map[string]string // name -> version
	category map[string]string
	headers  string
	shot     bool
	ips      string
	cname    string
}

type world struct {
	st       *store.Store
	programs map[string]uuid.UUID
}

func (w *world) program(t *testing.T, name, slug string) {
	t.Helper()
	var id uuid.UUID
	if err := w.st.Pool.QueryRow(context.Background(),
		`INSERT INTO program (origin, platform, slug, name, created_at, updated_at) VALUES ('scopesextractor', 'hackerone', $1, $2, now(), now()) RETURNING id`,
		slug, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	w.programs[slug] = id
}

func (w *world) add(t *testing.T, s svc) {
	t.Helper()
	ctx := context.Background()
	pid := w.programs[s.program]
	key := fmt.Sprintf("%s:%d/tcp", s.host, s.port)
	if s.state == "" {
		s.state = "active"
	}
	first := now.Add(-s.age)
	var hostID, id uuid.UUID
	// The host, then the service on it.
	if err := w.st.Pool.QueryRow(ctx, `INSERT INTO asset (program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1, 'fqdn', $2, $2, 'fastrecon', 'in_scope', $3, $3)
		ON CONFLICT (program_id, kind, key) DO UPDATE SET last_seen = EXCLUDED.last_seen RETURNING id`, pid, s.host, first).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.Pool.Exec(ctx, `INSERT INTO asset_current (asset_id, program_id, kind, key, host, scope_status, lifecycle, first_seen, last_seen, lifecycle_since, ips, cname)
		VALUES ($1, $2, 'fqdn', $3, $3, 'in_scope', 'active', $4, $4, $4, $5::inet[], $6::text[]) ON CONFLICT DO NOTHING`,
		hostID, pid, s.host, first, pgArray(s.ips), pgArray(s.cname)); err != nil {
		t.Fatal(err)
	}
	if err := w.st.Pool.QueryRow(ctx, `INSERT INTO asset (program_id, kind, key, host, port, scheme, parent_asset_id, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1, 'service', $2, $3, $4, 'https', $5, 'fastrecon', 'in_scope', $6, $6) RETURNING id`, pid, key, s.host, s.port, hostID, first).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.Pool.Exec(ctx, `INSERT INTO asset_current (asset_id, program_id, kind, key, host, port, scheme, scope_status, lifecycle,
		  http_state, status_code, title, server, cdn, favicon_hash, headers, first_seen, last_seen, lifecycle_since, ips)
		VALUES ($1, $2, 'service', $3, $4, $5, 'https', 'in_scope', $6, 'responding', $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''),
		  NULLIF($11, ''), NULLIF($12, '')::jsonb, $13, $13, $13, $14::inet[])`,
		id, pid, key, s.host, s.port, s.state, s.status, s.title, s.server, s.cdn, s.favicon, s.headers, first, pgArray(s.ips)); err != nil {
		t.Fatal(err)
	}
	for name, v := range s.techs {
		cat := s.category[name]
		if _, err := w.st.Pool.Exec(ctx, `INSERT INTO asset_tech (asset_id, name, category, version_raw, version) VALUES ($1, $2, $3, $4, $5::int[])`,
			id, name, cat, v, fingerprint.ParseVersion(v)); err != nil {
			t.Fatal(err)
		}
	}
	if s.triage != "" {
		if _, err := w.st.Pool.Exec(ctx, `INSERT INTO triage (asset_id, state) VALUES ($1, $2)`, id, s.triage); err != nil {
			t.Fatal(err)
		}
	}
	if s.shot {
		if _, err := w.st.Pool.Exec(ctx, `INSERT INTO screenshot (asset_id, key, thumb_key, phash, bytes, width, height, taken_at)
			VALUES ($1, $2, $3, 0, 1, 1, 1, now())`, id, id.String()+".jpg", id.String()+"t.jpg"); err != nil {
			t.Fatal(err)
		}
	}
}

func pgArray(csv string) *string {
	if csv == "" {
		return nil
	}
	s := "{" + csv + "}"
	return &s
}

func setup(t *testing.T) *world {
	t.Helper()
	w := &world{st: storetest.New(t), programs: map[string]uuid.UUID{}}
	w.program(t, "Acme Corp", "acme")
	w.program(t, "Foo", "foo")
	day := 24 * time.Hour
	for _, s := range []svc{
		{
			program: "acme", host: "app.acme.com", port: 8443, status: 200, title: "Admin Dashboard", server: "nginx/1.24.0",
			favicon: "-1293034", age: 2 * day, shot: true, techs: map[string]string{"Nginx": "1.24.0", "PHP": "8.1.2"},
			category: map[string]string{"Nginx": "Web servers"}, headers: `{"x-powered-by":"PHP/8.1.2"}`, ips: "10.1.2.3",
		},
		{
			program: "acme", host: "www.acme.com", port: 443, status: 200, title: "Sign in", cdn: "cloudflare", age: 3 * day,
			techs: map[string]string{"Nginx": "1.24.3", "WordPress": "6.1.2"}, cname: "acme.cdn.cloudflare.net",
		},
		{
			program: "acme", host: "dev1.acme.com", port: 8080, status: 403, title: "Forbidden", favicon: "-1293034", age: 5 * day,
			triage: "ignored", techs: map[string]string{"Nginx": ""},
		},
		{program: "acme", host: "dev2.acme.com", port: 8080, status: 401, title: "Login required", favicon: "-1293034", age: 6 * day},
		{program: "acme", host: "eviltarget.com", port: 443, status: 500, title: "Error", age: 10 * day},
		{
			program: "foo", host: "old.foo.io", port: 443, status: 401, favicon: "-1293034", age: 30 * day,
			techs: map[string]string{"WordPress": "4.9"},
		},
		{
			program: "acme", host: "jenkins.acme.com", port: 8080, status: 200, title: "Dashboard [Jenkins]", age: 8 * day,
			techs: map[string]string{"Jenkins": "2.401"},
		},
		{program: "acme", host: "docker.acme.com", port: 2375, status: 404, age: 1 * day, state: "candidate"},
		{program: "acme", host: "gone.acme.com", port: 6443, status: 0, age: 40 * day, state: "inactive"},
		{program: "acme", host: "es.acme.com", port: 9200, status: 200, age: 4 * day},
		{program: "acme", host: "kibana.acme.com", port: 5601, status: 302, age: 4 * day},
	} {
		w.add(t, s)
	}
	// The version table, one nginx per version on its own host.
	for _, v := range []string{"1.2", "1.9", "1.23.9", "1.24", "1.24.9", "1.25", "1.25.0", "1.25.1", "1.30", "2.0"} {
		w.add(t, svc{
			program: "foo", host: "nginx-" + strings.ReplaceAll(v, ".", "-") + ".v.test", port: 443, status: 200, age: 90 * day,
			techs: map[string]string{"Nginx": v},
		})
	}
	for _, v := range []string{"5.0", "6.1.5", "6.2"} {
		w.add(t, svc{
			program: "foo", host: "wp-" + strings.ReplaceAll(v, ".", "-") + ".v.test", port: 443, status: 200, age: 90 * day,
			techs: map[string]string{"WordPress": v},
		})
	}
	for _, v := range []string{"7.4", "8.0.1", "8.1"} {
		w.add(t, svc{
			program: "foo", host: "php-" + strings.ReplaceAll(v, ".", "-") + ".v.test", port: 443, status: 200, age: 90 * day,
			techs: map[string]string{"PHP": v},
		})
	}
	w.add(t, svc{program: "foo", host: "jq.v.test", port: 443, status: 200, age: 90 * day, techs: map[string]string{"jQuery": ""}})
	return w
}

func (w *world) keys(t *testing.T, q, view string) []string {
	t.Helper()
	var out []string
	req := search.Request{Query: q, View: view, Limit: 500, Now: now}
	for {
		page, err := search.Search(context.Background(), w.st.Pool, req)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for _, r := range page.Rows {
			out = append(out, r.Key)
		}
		if page.Next == "" {
			break
		}
		req.Cursor = page.Next
	}
	slices.Sort(out)
	return out
}

func hosts(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		h, _, _ := strings.Cut(k, ":")
		out = append(out, h)
	}
	return out
}

// The version table of the search page, row by row.
func TestVersionTable(t *testing.T) {
	w := setup(t)
	nginx := func(vs ...string) []string {
		var out []string
		for _, v := range vs {
			out = append(out, "nginx-"+strings.ReplaceAll(v, ".", "-")+".v.test")
		}
		return out
	}
	for q, want := range map[string][]string{
		// The rows of the table on the page, on the hosts that carry one version each.
		"tech:nginx@1.24 apex:v.test":         nginx("1.24", "1.24.9"),
		"tech:nginx@1.24.0 apex:v.test":       nil,
		"tech:nginx@<1.25 apex:v.test":        nginx("1.2", "1.9", "1.23.9", "1.24", "1.24.9"),
		"tech:nginx@<=1.24 apex:v.test":       nginx("1.2", "1.9", "1.23.9", "1.24", "1.24.9"),
		"tech:nginx@>1.24 apex:v.test":        nginx("1.25", "1.25.0", "1.25.1", "1.30", "2.0"),
		"tech:nginx@>=1.24 apex:v.test":       nginx("1.24", "1.24.9", "1.25", "1.25.0", "1.25.1", "1.30", "2.0"),
		"tech:wordpress@5.0..6.1 apex:v.test": {"wp-5-0.v.test", "wp-6-1-5.v.test"},
		"tech:php@7,8.0 apex:v.test":          {"php-7-4.v.test", "php-8-0-1.v.test"},
		"tech:jquery@? apex:v.test":           {"jq.v.test"},
		"tech:nginx apex:v.test":              nginx("1.2", "1.9", "1.23.9", "1.24", "1.24.9", "1.25", "1.25.0", "1.25.1", "1.30", "2.0"),
	} {
		got := hosts(w.keys(t, q, ""))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s\n  got  %v\n  want %v", q, got, want)
		}
	}
	// 1.24.0 matches itself, not 1.24 (the row of the table).
	if got := hosts(w.keys(t, "tech:nginx@1.24.0", "")); !slices.Equal(got, []string{"app.acme.com"}) {
		t.Errorf("@1.24.0 = %v", got)
	}
	// A version is bound to its own technology: PHP 8.1 does not make nginx 8.1.
	if got := w.keys(t, "tech:nginx@8.1", ""); len(got) != 0 {
		t.Errorf("@8.1 on nginx matched %v", got)
	}
}

func TestPageExamples(t *testing.T) {
	w := setup(t)
	for q, want := range map[string][]string{
		"tech:nginx@<1.25 status:2xx port:8080,8443 -cdn:cloudflare": {"app.acme.com:8443/tcp"},
		"tech:wordpress@5.0..6.1 program:acme* first_seen:<7d":       {"www.acme.com:443/tcp"},
		"(status:401 OR status:403) has:favicon -triage:ignored":     {"dev2.acme.com:8080/tcp", "old.foo.io:443/tcp"},
		"server:nginx*":                                      {"app.acme.com:8443/tcp"},
		"title:*admin*":                                      {"app.acme.com:8443/tcp"},
		`title:"Sign in"`:                                    {"www.acme.com:443/tcp"},
		"status:>=500":                                       {"eviltarget.com:443/tcp"},
		"status:400..499 program:acme":                       {"dev1.acme.com:8080/tcp", "dev2.acme.com:8080/tcp", "docker.acme.com:2375/tcp"},
		`host:/^dev[0-9]+\./`:                                {"dev1.acme.com:8080/tcp", "dev2.acme.com:8080/tcp"},
		"has:screenshot":                                     {"app.acme.com:8443/tcp"},
		"(port:9200 OR port:5601)":                           {"es.acme.com:9200/tcp", "kibana.acme.com:5601/tcp"},
		"status:4xx,5xx program:foo":                         {"old.foo.io:443/tcp"},
		"apex:acme.com port:8443":                            {"app.acme.com:8443/tcp"},
		"apex:target.com":                                    nil, // eviltarget.com is not under target.com
		"cname:*.cloudflare.net":                             nil, // cname lives on hosts, the services view has none
		"header.x-powered-by:PHP*":                           {"app.acme.com:8443/tcp"},
		"category:\"Web servers\"":                           {"app.acme.com:8443/tcp"},
		"ip:10.0.0.0/8":                                      {"app.acme.com:8443/tcp"},
		"favicon:-1293034 program:acme":                      {"app.acme.com:8443/tcp", "dev1.acme.com:8080/tcp", "dev2.acme.com:8080/tcp"},
		"triage:ignored":                                     {"dev1.acme.com:8080/tcp"},
		"tech:jenkins@<2.440":                                {"jenkins.acme.com:8080/tcp"},
		"port:2375,2376,6443,10250 -state:inactive,archived": {"docker.acme.com:2375/tcp"},
		"port:3306,5432,27017,6379,9200 -state:inactive,archived": {"es.acme.com:9200/tcp"},
		`title:*login*,"*sign in*" first_seen:<7d`:                {"dev2.acme.com:8080/tcp", "www.acme.com:443/tcp"},
		"first_seen:>7d program:acme":                             {"eviltarget.com:443/tcp", "gone.acme.com:6443/tcp", "jenkins.acme.com:8080/tcp"},
	} {
		got := w.keys(t, q, "")
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s\n  got  %v\n  want %v", q, got, want)
		}
	}
	// cname is on the host: the hosts and assets views see it.
	if got := w.keys(t, "cname:*.cloudflare.net", search.ViewAssets); !slices.Equal(got, []string{"www.acme.com"}) {
		t.Errorf("cname in the assets view = %v", got)
	}
	// The hosts view folds matching services into their host.
	page, err := search.Search(context.Background(), w.st.Pool, search.Request{Query: "port:8080 program:acme", View: search.ViewHosts, Limit: 50, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 3 || page.Rows[0].Kind != "fqdn" || len(page.Rows[0].Services) != 1 {
		t.Errorf("hosts view = %+v", page.Rows)
	}
}

// Every query on the search page parses, prints back to itself, compiles and
// runs: a new example on the page that the language does not accept fails here.
func TestEveryQueryOnThePageRuns(t *testing.T) {
	w := setup(t)
	doc, err := os.ReadFile("../../docs/src/content/docs/architecture/search.md")
	if err != nil {
		t.Fatal(err)
	}
	// Inline code spans holding field:value, and the lines of the first code block.
	span := regexp.MustCompile("`([^`\n]*[a-z_]+:[^`\n]*)`")
	var queries []string
	for _, m := range span.FindAllStringSubmatch(string(doc), -1) {
		queries = append(queries, m[1])
	}
	block := regexp.MustCompile("(?s)```\n(.*?)```")
	if b := block.FindStringSubmatch(string(doc)); b != nil {
		queries = append(queries, strings.Split(strings.TrimSpace(b[1]), "\n")...)
	}
	ran := 0
	for _, q := range queries {
		// Grammar fragments and field names in prose are not queries.
		if strings.Contains(q, "=") || strings.HasPrefix(q, "GET ") || strings.Contains(q, "<name>") || strings.Contains(q, "<value>") ||
			strings.HasPrefix(q, "change:") || q == "has:field" || strings.HasSuffix(q, ":") || strings.HasPrefix(q, "field:") ||
			strings.HasPrefix(q, "tech:name") {
			continue
		}
		n, err := search.Parse(q)
		if err != nil {
			t.Errorf("%q does not parse: %v", q, err)
			continue
		}
		if again, err := search.Parse(n.String()); err != nil || again.String() != n.String() {
			t.Errorf("%q does not print back: %q", q, n.String())
		}
		if _, err := search.Search(context.Background(), w.st.Pool, search.Request{Query: q, Limit: 5, Now: now}); err != nil {
			t.Errorf("%q does not run: %v", q, err)
			continue
		}
		ran++
	}
	if ran < 40 {
		t.Errorf("only %d queries found on the page: the extraction broke", ran)
	}
}

func TestRefusalsNameWhatIsWrong(t *testing.T) {
	w := setup(t)
	for q, kind := range map[string]string{
		"techs:nginx":       "unknown_field",
		"status:abc":        "bad_value",
		"state:alive":       "bad_value",
		"title:>5":          "bad_value",
		"host:/(/":          "bad_value",
		"has:everything":    "unknown_field",
		"first_seen:monday": "bad_value",
		"tech:nginx@latest": "bad_value",
		"scope:inside":      "bad_value",
	} {
		_, err := search.Search(context.Background(), w.st.Pool, search.Request{Query: q, Limit: 5, Now: now})
		var qe *search.QueryError
		if !asQueryError(err, &qe) || qe.Kind != kind {
			t.Errorf("%s: err = %v, want %s", q, err, kind)
		}
	}
	_, err := search.Search(context.Background(), w.st.Pool, search.Request{Query: "techs:nginx", Limit: 5, Now: now})
	var qe *search.QueryError
	if asQueryError(err, &qe) && qe.Suggest != "tech" {
		t.Errorf("suggest = %q, want tech", qe.Suggest)
	}
}

func asQueryError(err error, target **search.QueryError) bool {
	qe, ok := err.(*search.QueryError) //nolint:errorlint // the search package returns it unwrapped
	if ok {
		*target = qe
	}
	return ok
}

func TestCursorWalksEverythingOnceAndIsTiedToItsView(t *testing.T) {
	w := setup(t)
	all := w.keys(t, "", "")
	req := search.Request{Limit: 3, Now: now}
	var walked []string
	for {
		page, err := search.Search(context.Background(), w.st.Pool, req)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Rows {
			walked = append(walked, r.Key)
		}
		if page.Next == "" {
			break
		}
		if _, err := search.Search(context.Background(), w.st.Pool, search.Request{View: search.ViewHosts, Cursor: page.Next, Limit: 3, Now: now}); err != search.ErrBadCursor { //nolint:errorlint // sentinel
			t.Fatalf("a services cursor in the hosts view: %v", err)
		}
		req.Cursor = page.Next
	}
	slices.Sort(walked)
	if !slices.Equal(walked, all) {
		t.Errorf("paging by 3 walked %d rows, a single page has %d", len(walked), len(all))
	}
}

func TestFacets(t *testing.T) {
	w := setup(t)
	f, err := search.Facets(context.Background(), w.st.Pool, search.Request{Query: "program:acme", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	find := func(name, value string) int {
		for _, c := range f[name] {
			if c.Value == value {
				return c.Count
			}
		}
		return -1
	}
	if n := find("technologies", "Nginx"); n != 3 {
		t.Errorf("Nginx facet = %d, want 3", n)
	}
	if n := find("status", "4xx"); n != 3 {
		t.Errorf("4xx facet = %d, want 3", n)
	}
	for _, c := range f["technologies"] {
		if _, err := search.Parse(c.Term); err != nil {
			t.Errorf("facet term %q does not parse", c.Term)
		}
	}
}

func TestNegatedKindKeepsTheView(t *testing.T) {
	w := setup(t)
	for _, k := range w.keys(t, "-kind:url program:acme", "") {
		if !strings.Contains(k, "/tcp") {
			t.Fatalf("-kind:url in the services view returned %q, not a service", k)
		}
	}
}

func TestHostsViewFoldsUnderAddresses(t *testing.T) {
	w := setup(t)
	w.add(t, svc{program: "acme", host: "10.0.0.5", port: 8443, status: 200, title: "Appliance", age: time.Hour})
	if _, err := w.st.Pool.Exec(context.Background(), `UPDATE asset SET kind = 'ip' WHERE key = '10.0.0.5';
		UPDATE asset_current SET kind = 'ip' WHERE key = '10.0.0.5'`); err != nil {
		t.Fatal(err)
	}
	page, err := search.Search(context.Background(), w.st.Pool, search.Request{Query: "title:Appliance", View: search.ViewHosts, Limit: 10, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Key != "10.0.0.5" || len(page.Rows[0].Services) != 1 {
		t.Errorf("a service on an address host in the hosts view = %+v", page.Rows)
	}
}

func TestLightExportSkipsWhatItDoesNotPrint(t *testing.T) {
	w := setup(t)
	var full, light []search.Row
	req := search.Request{Query: "has:screenshot", Now: now}
	if err := search.Export(context.Background(), w.st.Pool, req, false, func(r search.Row) error { full = append(full, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := search.Export(context.Background(), w.st.Pool, req, true, func(r search.Row) error { light = append(light, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(full) != 1 || len(light) != 1 || full[0].Thumbnail == nil || light[0].Thumbnail != nil || light[0].URL() != full[0].URL() {
		t.Errorf("full %+v\nlight %+v", full, light)
	}
}
