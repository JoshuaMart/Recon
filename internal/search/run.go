package search

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Views.
const (
	ViewServices = "services"
	ViewHosts    = "hosts"
	ViewAssets   = "assets"
)

// Row is one result.
type Row struct {
	AssetID      uuid.UUID  `json:"asset_id"`
	ProgramID    uuid.UUID  `json:"program_id"`
	Program      string     `json:"program"`
	Kind         string     `json:"kind"`
	Key          string     `json:"key"`
	Host         string     `json:"host"`
	Port         *int32     `json:"port"`
	Scheme       *string    `json:"scheme"`
	Scope        string     `json:"scope"`
	Lifecycle    string     `json:"lifecycle"`
	StatusCode   *int32     `json:"status_code"`
	Title        *string    `json:"title"`
	Server       *string    `json:"server"`
	FinalURL     *string    `json:"final_url"`
	CDN          *string    `json:"cdn"`
	WAF          *string    `json:"waf"`
	Country      *string    `json:"country"`
	Technologies []string   `json:"technologies"`
	Thumbnail    *string    `json:"thumbnail"`
	Triage       string     `json:"triage"`
	FirstSeen    time.Time  `json:"first_seen"`
	LastSeen     time.Time  `json:"last_seen"`
	LastChanged  *time.Time `json:"last_changed"`
	// Services are, in the hosts view, the host's matching services. A host is
	// a name or an address.
	Services []string `json:"services,omitempty"`
}

// Page is a page of results and the cursor of the next one.
type Page struct {
	Query string `json:"query"`
	View  string `json:"view"`
	Rows  []Row  `json:"rows"`
	Next  string `json:"next,omitempty"`
	Limit int    `json:"limit"`
}

// ErrBadCursor is a cursor that does not belong to this view, or that does
// not decode: a walk that would restart or skip, refused rather than served.
var ErrBadCursor = errors.New("cursor does not belong to this view")

// Request is a search.
type Request struct {
	Query  string
	View   string
	Cursor string
	Limit  int
	Now    time.Time
}

const selectRow = `ac.asset_id, ac.program_id, p.name, ac.kind, ac.key, ac.host, ac.port, ac.scheme,
  ac.scope_status, ac.lifecycle, ac.status_code, ac.title, ac.server, ac.final_url, ac.cdn, ac.waf, ac.country,
  COALESCE((SELECT array_agg(t.name || COALESCE('@' || NULLIF(t.version_raw, ''), '') ORDER BY t.name)
            FROM asset_tech t WHERE t.asset_id = ac.asset_id), '{}'),
  (SELECT s.thumb_key FROM screenshot s WHERE s.asset_id = ac.asset_id ORDER BY s.taken_at DESC, s.id DESC LIMIT 1),
  COALESCE(tr.state, 'new'), ac.first_seen, ac.last_seen, ac.last_changed`

func scanRow(rows pgx.Rows, extra ...any) (Row, error) {
	var r Row
	dest := []any{
		&r.AssetID, &r.ProgramID, &r.Program, &r.Kind, &r.Key, &r.Host, &r.Port, &r.Scheme,
		&r.Scope, &r.Lifecycle, &r.StatusCode, &r.Title, &r.Server, &r.FinalURL, &r.CDN, &r.WAF, &r.Country,
		&r.Technologies, &r.Thumbnail, &r.Triage, &r.FirstSeen, &r.LastSeen, &r.LastChanged,
	}
	err := rows.Scan(append(dest, extra...)...)
	return r, err
}

// prepare parses and compiles, and adds the view's narrowing.
func prepare(req Request) (Node, Compiled, error) {
	n, err := Parse(req.Query)
	if err != nil {
		return nil, Compiled{}, err
	}
	c, err := Compile(n, req.Now)
	if err != nil {
		return nil, Compiled{}, err
	}
	if req.View == ViewServices && (n == nil || !MentionsKind(n)) {
		c.Where = "(" + c.Where + ") AND ac.kind IN ('service', 'url')"
	}
	return n, c, nil
}

func encodeCursor(view string, t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(view + "|" + t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(view, s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.UUID{}, ErrBadCursor
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != view {
		return time.Time{}, uuid.UUID{}, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return time.Time{}, uuid.UUID{}, ErrBadCursor
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return time.Time{}, uuid.UUID{}, ErrBadCursor
	}
	return t, id, nil
}

// Search runs one page, newest first.
func Search(ctx context.Context, pool *pgxpool.Pool, req Request) (Page, error) {
	switch req.View {
	case "":
		req.View = ViewServices
	case ViewServices, ViewHosts, ViewAssets:
	default:
		return Page{}, &QueryError{Kind: "bad_value", Field: "view", Detail: "view is services, hosts or assets"}
	}
	req.Limit = min(max(req.Limit, 1), 500)
	n, c, err := prepare(req)
	if err != nil {
		return Page{}, err
	}
	page := Page{View: req.View, Limit: req.Limit, Rows: []Row{}}
	if n != nil {
		page.Query = n.String()
	}

	args := c.Args
	after := ""
	if req.Cursor != "" {
		t, id, err := decodeCursor(req.View, req.Cursor)
		if err != nil {
			return Page{}, err
		}
		args = append(args, t, id)
		after = fmt.Sprintf(" AND (ac.first_seen, ac.asset_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, req.Limit+1)
	lim := "$" + strconv.Itoa(len(args))

	var sql string
	if req.View == ViewHosts {
		sql = `WITH m AS (SELECT ac.program_id, ac.host, ac.kind, ac.key FROM ` + From + ` WHERE ` + c.Where + `)
SELECT ` + selectRow + `,
  (SELECT array_agg(m2.key ORDER BY m2.key) FROM m m2 WHERE m2.program_id = ac.program_id AND m2.host = ac.host AND m2.kind IN ('service', 'url'))
FROM ` + From + `
WHERE ac.kind IN ('fqdn', 'ip') AND EXISTS (SELECT 1 FROM m WHERE m.program_id = ac.program_id AND m.host = ac.host)` +
			after + `
ORDER BY ac.first_seen DESC, ac.asset_id DESC LIMIT ` + lim
	} else {
		sql = `SELECT ` + selectRow + ` FROM ` + From + ` WHERE ` + c.Where + after +
			` ORDER BY ac.first_seen DESC, ac.asset_id DESC LIMIT ` + lim
	}
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var services []string
		var r Row
		if req.View == ViewHosts {
			r, err = scanRow(rows, &services)
			r.Services = services
		} else {
			r, err = scanRow(rows)
		}
		if err != nil {
			return Page{}, err
		}
		page.Rows = append(page.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Rows) > req.Limit {
		last := page.Rows[req.Limit-1]
		page.Rows = page.Rows[:req.Limit]
		page.Next = encodeCursor(req.View, last.FirstSeen, last.AssetID)
	}
	return page, nil
}

// Count is one facet value.
type Count struct {
	Value string `json:"value"`
	Count int    `json:"count"`
	// Term is what clicking it adds to the query.
	Term string `json:"term"`
}

// facets are computed over the matching rows. Each is a column expression,
// the term it turns into, and how many values it shows.
var facets = []struct {
	name, expr, term string
	join             string
}{
	{"technologies", "t.name", "tech", "JOIN asset_tech t ON t.asset_id = m.asset_id"},
	{"status", "(ac.status_code / 100)::text || 'xx'", "status", ""},
	{"ports", "ac.port::text", "port", ""},
	{"programs", "p.name", "program", ""},
	{"cdn", "ac.cdn", "cdn", ""},
	{"state", "ac.lifecycle", "state", ""},
	{"triage", "COALESCE(tr.state, 'new')", "triage", ""},
}

// Facets counts the matching rows per value of each facet, the 20 most
// frequent of each, in one query: the filter is evaluated once, into a
// materialized set every facet counts over.
func Facets(ctx context.Context, pool *pgxpool.Pool, req Request) (map[string][]Count, error) {
	if req.View == "" {
		req.View = ViewServices
	}
	_, c, err := prepare(req)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(facets))
	for i, f := range facets {
		parts = append(parts, fmt.Sprintf(`SELECT %d AS f, %s AS v, count(DISTINCT m.asset_id) AS n FROM m
JOIN asset_current ac ON ac.asset_id = m.asset_id
JOIN program p ON p.id = ac.program_id
LEFT JOIN triage tr ON tr.asset_id = m.asset_id %s
WHERE %s IS NOT NULL GROUP BY 2`, i, f.expr, f.join, f.expr))
	}
	sql := `WITH m AS MATERIALIZED (SELECT ac.asset_id FROM ` + From + ` WHERE ` + c.Where + `),
counts AS (` + strings.Join(parts, "\nUNION ALL\n") + `)
SELECT f, v, n FROM (
  SELECT f, v, n, row_number() OVER (PARTITION BY f ORDER BY n DESC, v) AS rank FROM counts
) ranked WHERE rank <= 20 ORDER BY f, n DESC, v`
	rows, err := pool.Query(ctx, sql, c.Args...)
	if err != nil {
		return nil, fmt.Errorf("facets: %w", err)
	}
	defer rows.Close()
	out := map[string][]Count{}
	for _, f := range facets {
		out[f.name] = []Count{}
	}
	for rows.Next() {
		var i, n int
		var v string
		if err := rows.Scan(&i, &v, &n); err != nil {
			return nil, err
		}
		f := facets[i]
		out[f.name] = append(out[f.name], Count{Value: v, Count: n, Term: Term{Field: f.term, Values: []Value{{Lit: v}}}.String()})
	}
	return out, rows.Err()
}

// Validate parses and compiles a request without running it, so a refusal can
// be answered before a streamed body starts.
func Validate(req Request) error {
	_, _, err := prepare(req)
	return err
}

// MaxExport bounds an export.
const MaxExport = 100_000

// selectLight is selectRow without its correlated subqueries, for exports of
// URLs or hosts that use neither technologies nor thumbnails.
const selectLight = `ac.asset_id, ac.program_id, p.name, ac.kind, ac.key, ac.host, ac.port, ac.scheme,
  ac.scope_status, ac.lifecycle, ac.status_code, ac.title, ac.server, ac.final_url, ac.cdn, ac.waf, ac.country,
  '{}'::text[], NULL::text, COALESCE(tr.state, 'new'), ac.first_seen, ac.last_seen, ac.last_changed`

// Export streams every matching row to fn, up to MaxExport. light skips the
// technologies and thumbnails.
func Export(ctx context.Context, pool *pgxpool.Pool, req Request, light bool, fn func(Row) error) error {
	if req.View == "" {
		req.View = ViewServices
	}
	_, c, err := prepare(req)
	if err != nil {
		return err
	}
	args := append(c.Args, MaxExport)
	cols := selectRow
	if light {
		cols = selectLight
	}
	rows, err := pool.Query(ctx, `SELECT `+cols+` FROM `+From+` WHERE `+c.Where+
		` ORDER BY ac.first_seen DESC, ac.asset_id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// URL is a row's web address: a service's root or a declared URL.
func (r Row) URL() string {
	switch r.Kind {
	case "url":
		return r.Key
	case "service":
		scheme := "https"
		if r.Scheme != nil && *r.Scheme != "" {
			scheme = *r.Scheme
		}
		host := r.Host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		defaultPort := (scheme == "https" && r.Port != nil && *r.Port == 443) || (scheme == "http" && r.Port != nil && *r.Port == 80)
		if r.Port != nil && !defaultPort {
			host += ":" + strconv.Itoa(int(*r.Port))
		}
		return scheme + "://" + host + "/"
	}
	return ""
}
