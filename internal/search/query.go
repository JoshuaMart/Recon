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

// MaxLimit bounds a page. The export walks instead of accumulating, and a page
// of ten thousand rows holds a connection while it serializes.
const MaxLimit = 500

// Querier is what the search runs on: a transaction already scoped to an
// organization. A transaction, not a pool, because the policies read a
// transaction-scoped variable. The compiler also emits the organization clause;
// the two are not redundant (one is testable here, the other the guarantee the
// compiler cannot remove from itself).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Request is one question about an inventory.
type Request struct {
	Filter Node
	Limit  int
	Cursor Cursor
	// Display asks for the two badge filters to be evaluated. The list wants them;
	// the export must not, since a file does not say what it omits and a display
	// filter there would hide the denylist rule's effect.
	Display bool
}

// Cursor is where a walk left off. The list orders on (last_seen DESC,
// asset_id), the index it paginates on; an offset would rescan everything
// before the page and make a million-row export quadratic.
type Cursor struct {
	LastSeen time.Time
	AssetID  uuid.UUID
}

// Zero reports whether a walk is starting.
func (c Cursor) Zero() bool { return c.AssetID == uuid.Nil }

// String encodes a cursor for a client to hand back. It is opaque on purpose: a
// cursor a client can build gets built by hand, and then the ordering key
// cannot change.
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
	// Badge is display only, and absent unless a display was asked for. A
	// denylisted value, or one leading only to itself, is not worth a line in a row
	// scanned in a second; it stays searchable and counted, so this removes a badge,
	// not data.
	//
	// A pointer so the export can leave it unanswered; a file cannot tell that from
	// "not worth a badge".
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
	// The three layer verdicts: death belongs to a layer, and "the name no longer
	// resolves" and "every probe failed" are different sentences that a lifecycle
	// alone cannot write.
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
	// Source and Lineage are why the asset is in the inventory. The source is the
	// one from its first appearance, which is what lineage asks.
	Source    string          `json:"discovery_source"`
	Lineage   json.RawMessage `json:"lineage,omitempty"`
	FirstSeen time.Time       `json:"first_seen"`
	LastSeen  time.Time       `json:"last_seen"`
	// LastFingerprintAt dates the fingerprinter's values. The service runs on five
	// triggers, so a technology can be weeks older than the last probe, and one
	// timestamp would claim the browser saw what the probe saw.
	LastFingerprintAt *time.Time `json:"last_fingerprint_at,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	LastChangedAt     *time.Time `json:"last_changed_at,omitempty"`
}

// Page is one answer.
type Page struct {
	Rows []Row `json:"assets"`
	// Favicons are the page's images keyed by hash, as on the grouped list; the
	// same sidebar renders beside both shapes and must not draw blanks under one.
	// Only when a display was asked for, since the export walks this with it off.
	Favicons map[string]string `json:"favicons,omitempty"`
	Next     string            `json:"next_cursor,omitempty"`
}

// selectColumns is what a row reads. All of it is evidence or a pivot: no
// composite score, severity or environment label, which cannot be determined
// from outside.
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

// lineageJoin brings the asset's own identity beside its projection. By primary
// key, so a lookup per page row. Lineage answers "why is this here", the one
// thing the projection lacks.
const lineageJoin = " JOIN asset a ON a.id = c.asset_id"

// pivotJoin attaches each row's pivots and counters in one lateral over the
// page, not a COUNT per displayed value. The counter is maintained on write.
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
	// Bound like every other value: the flag decides what the statement computes,
	// not what it says.
	args = append(args, req.Display)
	join := fmt.Sprintf(pivotJoin, len(args))
	if !req.Cursor.Zero() {
		// Written out, not as a row comparison: the keys run in opposite directions
		// (last_seen DESC, asset_id), which "(a, b) < (x, y)" cannot express.
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

	// One row beyond the page says there is a next one, without a second count.
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
	// Cut says values were left out. A truncated facet that looks complete makes
	// someone believe the inventory holds nine ports.
	Cut bool `json:"truncated"`
}

// FacetTerm is one value and how many assets carry it.
type FacetTerm struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// FacetLimit is how many values one facet returns.
const FacetLimit = 20

// FacetValuesLimit is how many values one facet returns when asked for alone,
// which the sidebar does when someone opens a cut facet. Ten times the sidebar
// cap, not the whole set: the cut must stay expressible, and technologies over
// a large perimeter reach the hundreds. It costs no more than the sidebar, as
// both aggregate the same filtered set.
const FacetValuesLimit = 200

// technologiesFacet is the one facet over an array; two places build its branch.
const technologiesFacet = "technologies"

// facets are the ones offered, in sidebar order. Nearly all are promoted columns. A facet over
// attributes was once refused on the belief it needed a GIN index that cannot group; the
// filter runs once into a CTE and every facet groups over that, so a key of the object costs
// what a column does. The favicon is the one pivot wanted as a list ("which icons does this
// perimeter share, and how many assets each" is the fastest identity signal); last, as it
// draws as a grid while the rows above read in one column.
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
	{"favicon_hash", "c.attributes->>'favicon_hash'"},
}

// FacetPage is the sidebar's answer.
type FacetPage struct {
	Facets []Facet `json:"facets"`
	// Favicons are the images the favicon facet refers to. They travel with it, not
	// from the page's rows: a facet ranks the whole filtered result while a page
	// shows fifty, so the most shared icon is routinely absent from the rows and
	// would draw as a blank square.
	Favicons map[string]string `json:"favicons,omitempty"`
}

// Facets aggregates over the filtered result, not the inventory: these are the
// side counters of an ASM interface. One statement, not one per facet, since the
// expensive half is the filter.
func Facets(ctx context.Context, q Querier, org uuid.UUID, filter Node) (FacetPage, error) {
	compiled, err := Compile(org, filter)
	if err != nil {
		return FacetPage{}, err
	}

	args := append([]any{}, compiled.Args...)
	args = append(args, FacetLimit+1)
	cap := fmt.Sprintf("$%d", len(args))

	parts := make([]string, 0, len(facets)+1)
	for _, facet := range facets {
		part, _ := facetBranch(facet.field, cap)
		parts = append(parts, part)
	}
	technologies, _ := facetBranch(technologiesFacet, cap)
	parts = append(parts, technologies)

	return aggregate(ctx, q, org, compiled, parts, args, FacetLimit)
}

// FacetValues answers one facet, deeper than the sidebar, which caps at twenty values and
// leaves everything below unreachable (a technology on twelve assets is filterable but not
// clickable). Same filtered set and field table as the sidebar: a name that is not a facet is
// refused, and never an expression reaching the statement.
func FacetValues(ctx context.Context, q Querier, org uuid.UUID, filter Node, field string) (FacetPage, error) {
	compiled, err := Compile(org, filter)
	if err != nil {
		return FacetPage{}, err
	}

	args := append([]any{}, compiled.Args...)
	args = append(args, FacetValuesLimit+1)
	branch, ok := facetBranch(field, fmt.Sprintf("$%d", len(args)))
	if !ok {
		return FacetPage{}, refuse("%q is not a facet", field)
	}

	return aggregate(ctx, q, org, compiled, []string{branch}, args, FacetValuesLimit)
}

// facetBranch is one field's aggregation over the filtered set. Parenthesised,
// because a LIMIT cannot sit between a branch and the following UNION (the
// error would name the UNION).
//
// The field is never the caller's string: it is matched against the table above
// and the expression written there reaches the statement.
func facetBranch(field, cap string) (string, bool) {
	// Counts assets, not elements: "how many of my assets run nginx".
	if field == technologiesFacet {
		return fmt.Sprintf(
			`(SELECT '%s' AS field, t AS value, count(*) AS total
			    FROM filtered c, unnest(c.technologies) AS t GROUP BY 2 ORDER BY 3 DESC, 2 LIMIT %s)`,
			technologiesFacet, cap), true
	}
	for _, facet := range facets {
		if facet.field == field {
			return fmt.Sprintf(
				`(SELECT '%s' AS field, %s AS value, count(*) AS total FROM filtered c
				   WHERE %s IS NOT NULL GROUP BY 2 ORDER BY 3 DESC, 2 LIMIT %s)`,
				facet.field, facet.expr, facet.expr, cap), true
		}
	}
	return "", false
}

// aggregate runs the branches and reads them back into facets.
func aggregate(
	ctx context.Context, q Querier, org uuid.UUID, compiled Compiled,
	parts []string, args []any, limit int,
) (FacetPage, error) {
	sql := "WITH filtered AS (SELECT * FROM asset_current c WHERE " + compiled.SQL + ") " +
		strings.Join(parts, " UNION ALL ")

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return FacetPage{}, fmt.Errorf("run the facets: %w", err)
	}
	defer rows.Close()

	byField := map[string]*Facet{}
	order := make([]string, 0, len(facets)+1)
	for rows.Next() {
		var field, value string
		var total int
		if err := rows.Scan(&field, &value, &total); err != nil {
			return FacetPage{}, fmt.Errorf("read a facet: %w", err)
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
		return FacetPage{}, fmt.Errorf("read the facets: %w", err)
	}

	page := FacetPage{Facets: make([]Facet, 0, len(order))}
	hashes := make([]string, 0, limit)
	for _, field := range order {
		facet := byField[field]
		// The extra value tells the cut, and it is said.
		if len(facet.Terms) > limit {
			facet.Terms = facet.Terms[:limit]
			facet.Cut = true
		}
		if field == "favicon_hash" {
			for _, term := range facet.Terms {
				hashes = append(hashes, term.Value)
			}
		}
		page.Facets = append(page.Facets, *facet)
	}

	images, err := faviconsByHash(ctx, q, org, hashes)
	if err != nil {
		return FacetPage{}, err
	}
	page.Favicons = images
	return page, nil
}
