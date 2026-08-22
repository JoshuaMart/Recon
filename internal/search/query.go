package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultLimit is a page.
const DefaultLimit = 50

// MaxLimit bounds what a caller may ask for in one page.
//
// The export walks rather than accumulating, so nobody needs a large page to
// read a large inventory, and a page of ten thousand rows is a request that
// holds a connection for as long as it takes to serialize them.
const MaxLimit = 500

// Querier is what the search runs on: a transaction already scoped to an
// organization.
//
// A transaction rather than a pool, because the policies read a variable that
// is transaction scoped. The compiler emits the organization clause as well,
// and the two are not redundant: one is structural and testable here, the other
// is the guarantee the compiler cannot remove from itself.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Request is one question about an inventory.
type Request struct {
	Filter Node
	Limit  int
	Cursor Cursor
	// Display asks for the two badge filters to be evaluated. The list wants
	// them and the export must not have them: a file does not say what it does
	// not contain, so an export applying a display filter would do exactly what
	// the denylist rule forbids while making it invisible.
	Display bool
}

// Cursor is where a walk left off.
//
// The list orders on (last_seen DESC, asset_id), which is the index it
// paginates on. An offset would cost a scan of everything before the page on
// every page, which is what makes an export of a million rows quadratic.
type Cursor struct {
	LastSeen time.Time
	AssetID  uuid.UUID
}

// Zero reports whether a walk is starting.
func (c Cursor) Zero() bool { return c.AssetID == uuid.Nil }

// String encodes a cursor for a client to hand back.
//
// Opaque rather than readable, and that is not obfuscation: a cursor a client
// can build is a cursor a client will build by hand, and then the ordering key
// cannot change without breaking it.
func (c Cursor) String() string {
	if c.Zero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.LastSeen.UTC().Format(time.RFC3339Nano) + " " + c.AssetID.String()))
}

// ParseCursor reads one back.
func ParseCursor(encoded string) (Cursor, error) {
	if encoded == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, refuse("the cursor is not one this list handed out")
	}
	instant, id, found := strings.Cut(string(raw), " ")
	if !found {
		return Cursor{}, refuse("the cursor is not one this list handed out")
	}
	at, err := time.Parse(time.RFC3339Nano, instant)
	if err != nil {
		return Cursor{}, refuse("the cursor is not one this list handed out")
	}
	asset, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, refuse("the cursor is not one this list handed out")
	}
	return Cursor{LastSeen: at, AssetID: asset}, nil
}

// Pivot is one value that links assets, with what it links.
type Pivot struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Count int    `json:"count"`
	// Badge is display only, and absent entirely when nobody asked for a
	// display. A value the denylist names, or one leading only to itself, is
	// not worth a line in a row scanned in under a second; it stays fully
	// searchable and fully counted, which is the difference between removing a
	// badge and removing data.
	//
	// A pointer so that the export can leave the question unanswered rather
	// than answering it false, which a file has no way to distinguish from "not
	// worth a badge".
	Badge *bool `json:"badge,omitempty"`
}

// Row is one asset, as the list and the export read it.
type Row struct {
	AssetID     uuid.UUID `json:"asset_id"`
	ProgramID   uuid.UUID `json:"program_id"`
	Kind        string    `json:"kind"`
	Key         string    `json:"key"`
	Host        *string   `json:"host,omitempty"`
	Port        *int32    `json:"port,omitempty"`
	Scheme      *string   `json:"scheme,omitempty"`
	Lifecycle   string    `json:"lifecycle"`
	ScopeStatus string    `json:"scope_status"`
	// The three layer verdicts, because death is a property of a layer rather
	// than of an asset and the row has to say which. "The name no longer
	// resolves" and "every probe failed" are two different sentences, and a row
	// carrying only the lifecycle can write neither.
	DNSState    *string        `json:"dns_state,omitempty"`
	TCPState    *string        `json:"tcp_state,omitempty"`
	HTTPState   *string        `json:"http_state,omitempty"`
	StatusCode  *int32         `json:"status_code,omitempty"`
	StatusChain []int32        `json:"status_chain,omitempty"`
	FinalURL    *string        `json:"final_url,omitempty"`
	Title       *string        `json:"title,omitempty"`
	Server      *string        `json:"server,omitempty"`
	IP          *netip.Addr    `json:"ip,omitempty"`
	ASN         *int32         `json:"asn,omitempty"`
	ASNOrg      *string        `json:"asn_org,omitempty"`
	Country     *string        `json:"country,omitempty"`
	City        *string        `json:"city,omitempty"`
	IsCDN       *bool          `json:"is_cdn,omitempty"`
	CDNProvider *string        `json:"cdn_provider,omitempty"`
	WAFDetected *bool          `json:"waf_detected,omitempty"`
	WAFVendor   *string        `json:"waf_vendor,omitempty"`
	Tech        []string       `json:"technologies"`
	Attributes  map[string]any `json:"attributes"`
	Volatility  int            `json:"volatility"`
	Pivots      []Pivot        `json:"pivots"`
	// Source and Lineage are why the asset is in the inventory at all. The
	// source is the one from its first appearance rather than the last, which
	// is exactly the question lineage asks.
	Source    string          `json:"discovery_source"`
	Lineage   json.RawMessage `json:"lineage,omitempty"`
	FirstSeen time.Time       `json:"first_seen"`
	LastSeen  time.Time       `json:"last_seen"`
	// LastFingerprintAt is why the fingerprinter's values carry their own date.
	// The service runs on five triggers, so a technology or a cookie can be
	// weeks older than the last probe, and a row that showed one timestamp
	// would be claiming the browser saw what the probe saw.
	LastFingerprintAt *time.Time `json:"last_fingerprint_at,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	LastChangedAt     *time.Time `json:"last_changed_at,omitempty"`
}

// Page is one answer.
type Page struct {
	Rows []Row `json:"assets"`
	// Favicons are the images of the page, keyed by hash, exactly as on the
	// grouped list. The same sidebar renders beside both shapes, so a facet that
	// draws icons under one and blank squares under the other is the same list
	// disagreeing with itself.
	//
	// Only when a display was asked for. The export walks this function with it
	// off and would otherwise pay for images it never writes.
	Favicons map[string]string `json:"favicons,omitempty"`
	Next     string            `json:"next_cursor,omitempty"`
}

// selectColumns is what a row reads.
//
// Every one of them is evidence or a pivot, and there is no composite score, no
// severity and no environment label: those cannot be determined from the
// outside anyway, and a field that is neither evidence nor a pivot does not earn
// its place.
const selectColumns = `
    c.asset_id, c.program_id, c.kind, c.key, c.host, c.port, c.scheme,
    c.lifecycle, c.scope_status, c.dns_state, c.tcp_state, c.http_state,
    c.status_code, c.status_chain, c.final_url,
    c.title, c.server, c.ip, c.asn, c.asn_org, c.country, c.city,
    c.is_cdn, c.cdn_provider, c.waf_detected, c.waf_vendor,
    c.technologies, c.attributes,
    volatility(c.change_buckets, c.buckets_day),
    COALESCE(pv.pivots, '[]'::jsonb),
    a.discovery_source, a.discovery_path,
    c.first_seen, c.last_seen, c.last_fingerprint_at, c.last_checked_at, c.last_changed_at`

// lineageJoin brings the asset's own identity beside its projection.
//
// By primary key, so it costs a lookup per row of a page. Lineage answers "why
// is this here", which is the question a row raises as soon as somebody does not
// recognize a name, and it is the one thing the projection does not carry.
const lineageJoin = " JOIN asset a ON a.id = c.asset_id"

// pivotJoin attaches each row's pivots and their counters.
//
// One lateral over a page rather than one COUNT per displayed value, which is
// unworkable as soon as a page shows dozens. The counter itself is maintained
// on write; this only reads it.
const pivotJoin = `
    LEFT JOIN LATERAL (
        SELECT jsonb_agg(jsonb_build_object(
                   'type',  p.pivot_type,
                   'value', p.pivot_value,
                   'count', COALESCE(k.count, 0),
                   -- Two display filters, and neither touches the data. A
                   -- counter of one is a pivot leading only to itself, and a
                   -- value on the denylist groups without discriminating.
                   'badge', CASE WHEN $%d::boolean
                                 THEN COALESCE(k.count, 0) > 1
                                      AND NOT generic_pivot(p.pivot_type, p.pivot_value)
                            END)
                   ORDER BY p.pivot_type, p.pivot_value) AS pivots
          FROM pivot_values(c.attributes) AS p(pivot_type, pivot_value)
          LEFT JOIN pivot_count k
                 ON k.org_id = c.org_id
                AND k.pivot_type = p.pivot_type
                AND k.pivot_value = p.pivot_value
    ) pv ON true`

// List answers one page of assets.
func List(ctx context.Context, q Querier, org uuid.UUID, req Request) (Page, error) {
	compiled, err := Compile(org, req.Filter)
	if err != nil {
		return Page{}, err
	}

	limit := req.Limit
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		return Page{}, refuse("a page of %d rows exceeds the bound of %d", limit, MaxLimit)
	}

	where := compiled.SQL
	args := compiled.Args
	// Bound rather than interpolated, like every other value: the flag decides
	// what the statement computes, not what it says.
	args = append(args, req.Display)
	join := fmt.Sprintf(pivotJoin, len(args))
	if !req.Cursor.Zero() {
		// Written out rather than as a row comparison, because the two keys
		// travel in opposite directions: the index is (last_seen DESC,
		// asset_id), and "(a, b) < (x, y)" cannot express that.
		args = append(args, req.Cursor.LastSeen, req.Cursor.AssetID)
		where += fmt.Sprintf(
			" AND (%[1]s.last_seen < $%[2]d OR (%[1]s.last_seen = $%[2]d AND %[1]s.asset_id > $%[3]d))",
			Alias, len(args)-1, len(args))
	}
	args = append(args, limit+1)

	sql := "SELECT" + selectColumns + " FROM asset_current c" + lineageJoin + join +
		" WHERE " + where +
		" ORDER BY c.last_seen DESC, c.asset_id" +
		fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return Page{}, fmt.Errorf("run the list: %w", err)
	}
	defer rows.Close()

	page := Page{Rows: make([]Row, 0, limit)}
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return Page{}, err
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("read the list: %w", err)
	}

	// One row beyond the page is how the walk knows there is a next one,
	// without a second count over the same predicate.
	if len(page.Rows) > limit {
		page.Rows = page.Rows[:limit]
		last := page.Rows[limit-1]
		page.Next = Cursor{LastSeen: last.LastSeen, AssetID: last.AssetID}.String()
	}

	if req.Display {
		images, err := Favicons(ctx, q, org, page.Rows)
		if err != nil {
			return Page{}, err
		}
		page.Favicons = images
	}
	return page, nil
}

func scanRow(rows pgx.Rows) (Row, error) {
	var row Row
	var pivots []byte
	var attributes []byte
	err := rows.Scan(
		&row.AssetID, &row.ProgramID, &row.Kind, &row.Key, &row.Host, &row.Port, &row.Scheme,
		&row.Lifecycle, &row.ScopeStatus, &row.DNSState, &row.TCPState, &row.HTTPState,
		&row.StatusCode, &row.StatusChain, &row.FinalURL,
		&row.Title, &row.Server, &row.IP, &row.ASN, &row.ASNOrg, &row.Country, &row.City,
		&row.IsCDN, &row.CDNProvider, &row.WAFDetected, &row.WAFVendor,
		&row.Tech, &attributes, &row.Volatility, &pivots,
		&row.Source, &row.Lineage,
		&row.FirstSeen, &row.LastSeen, &row.LastFingerprintAt, &row.LastCheckedAt, &row.LastChangedAt)
	if err != nil {
		return Row{}, fmt.Errorf("read a row: %w", err)
	}
	if err := json.Unmarshal(attributes, &row.Attributes); err != nil {
		return Row{}, fmt.Errorf("decode the attributes of %s: %w", row.Key, err)
	}
	if err := json.Unmarshal(pivots, &row.Pivots); err != nil {
		return Row{}, fmt.Errorf("decode the pivots of %s: %w", row.Key, err)
	}
	return row, nil
}

// Facet is one side counter of the filtered result.
type Facet struct {
	Field string      `json:"field"`
	Terms []FacetTerm `json:"terms"`
	// Cut says values were left out. A truncated facet that looks complete
	// makes somebody believe the inventory holds nine ports, which is the same
	// failure the export refuses and the timeline refuses.
	Cut bool `json:"truncated"`
}

// FacetTerm is one value and how many assets carry it.
type FacetTerm struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// FacetLimit is how many values one facet returns.
const FacetLimit = 20

// facets are the only ones offered, and every one of them is a promoted column
// with an index.
//
// A facet over attributes would have to aggregate through a GIN index that
// answers containment and cannot group, which is the whole reason this list is
// this one.
var facets = []struct{ field, expr string }{
	{"lifecycle", "c.lifecycle"},
	{"kind", "c.kind"},
	{"port", "c.port::text"},
	{"scheme", "c.scheme"},
	{"status_code", "c.status_code::text"},
	{"country", "c.country"},
	{"asn", "c.asn::text"},
	{"cdn_provider", "c.cdn_provider"},
	{"waf_vendor", "c.waf_vendor"},
}

// Facets aggregates over the filtered result rather than over the inventory.
//
// That is what the side counters of an ASM interface are, and it is what
// usually pushes a project toward a search engine. One statement rather than
// one per facet: the expensive half is the filter, and running it ten times is
// paying for it ten times.
func Facets(ctx context.Context, q Querier, org uuid.UUID, filter Node) ([]Facet, error) {
	compiled, err := Compile(org, filter)
	if err != nil {
		return nil, err
	}

	args := append([]any{}, compiled.Args...)
	args = append(args, FacetLimit+1)
	cap := fmt.Sprintf("$%d", len(args))

	parts := make([]string, 0, len(facets)+1)
	for _, facet := range facets {
		// Parenthesised, because a LIMIT cannot sit between a branch and the
		// UNION that follows it: without them the statement is a syntax error
		// naming the UNION rather than the limit.
		parts = append(parts, fmt.Sprintf(
			`(SELECT '%s' AS field, %s AS value, count(*) AS total FROM filtered c
			   WHERE %s IS NOT NULL GROUP BY 2 ORDER BY 3 DESC, 2 LIMIT %s)`,
			facet.field, facet.expr, facet.expr, cap))
	}
	// The one facet over an array. It counts assets rather than elements, which
	// is what the question asks: "how many of my assets run nginx".
	parts = append(parts, fmt.Sprintf(
		`(SELECT 'technologies' AS field, t AS value, count(*) AS total
		    FROM filtered c, unnest(c.technologies) AS t GROUP BY 2 ORDER BY 3 DESC, 2 LIMIT %s)`, cap))

	sql := "WITH filtered AS (SELECT * FROM asset_current c WHERE " + compiled.SQL + ") " +
		strings.Join(parts, " UNION ALL ")

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("run the facets: %w", err)
	}
	defer rows.Close()

	byField := map[string]*Facet{}
	order := make([]string, 0, len(facets)+1)
	for rows.Next() {
		var field, value string
		var total int
		if err := rows.Scan(&field, &value, &total); err != nil {
			return nil, fmt.Errorf("read a facet: %w", err)
		}
		facet, seen := byField[field]
		if !seen {
			facet = &Facet{Field: field}
			byField[field] = facet
			order = append(order, field)
		}
		facet.Terms = append(facet.Terms, FacetTerm{Value: value, Count: total})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the facets: %w", err)
	}

	out := make([]Facet, 0, len(order))
	for _, field := range order {
		facet := byField[field]
		// The extra value is how the cut is known, and it is said rather than
		// swallowed.
		if len(facet.Terms) > FacetLimit {
			facet.Terms = facet.Terms[:FacetLimit]
			facet.Cut = true
		}
		out = append(out, *facet)
	}
	return out, nil
}
