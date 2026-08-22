package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Grouping the list by host.
//
// The service is the unit of identity and every open port becomes an asset, so
// one name occupies several rows: the fqdn, https, http, and one per additional
// open port. Five ports of one address have the same ASN, the same geolocation
// and often the same certificate, so the repetition is a property of the model
// and has to be solved by the structure of the screen.
//
// Done here rather than in the page, and that is the load bearing half. Folding
// fifty already fetched assets breaks at the page boundary: a host whose
// services fall on either side renders as two partial groups with two wrong
// counts. So pagination is over hosts, and the cursor is a host cursor.

// Group is one host and the assets of the result that belong to it.
type Group struct {
	// Host is the value the members share. A row whose host column is null is
	// grouped under its own key, which shows it alone and in its place in the
	// order rather than pooling every such row under one name that means
	// nothing.
	Host string `json:"host"`
	// LastSeen is the most recent of the group, and what the groups are ordered
	// by: what moved recently stays at the top one level up, and folding must
	// not cost that.
	LastSeen time.Time `json:"last_seen"`
	Rows     []Row     `json:"assets"`
}

// GroupedPage is one answer of the folded list.
type GroupedPage struct {
	Groups []Group `json:"groups"`
	// Favicons are the images of the page, keyed by hash. On the page and not
	// on each row: a shared favicon is the interesting case, and repeating two
	// kilobytes per asset would undo the point of storing one copy.
	Favicons map[string]string `json:"favicons,omitempty"`
	Next     string            `json:"next_cursor,omitempty"`
}

// GroupCursor is where a walk over hosts left off.
//
// It carries the host itself rather than an asset identifier: two hosts can
// share a last_seen to the microsecond on a first scan, and the tie has to
// break on something stable.
type GroupCursor struct {
	LastSeen time.Time `json:"s"`
	Host     string    `json:"h"`
}

// String encodes it for a client to hand back.
//
// Opaque, and deliberately not the same encoding as the flat list's. A caller
// that swaps one for the other gets a refusal rather than a walk that restarts
// or skips, because neither decodes as the other.
func (c GroupCursor) String() string {
	if c.Host == "" {
		return ""
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// ParseGroupCursor reads one back.
func ParseGroupCursor(encoded string) (GroupCursor, error) {
	if encoded == "" {
		return GroupCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return GroupCursor{}, refuse("the cursor is not one this list handed out")
	}
	var cursor GroupCursor
	// The host is the half that has to be there. A zero timestamp is a
	// legitimate value nobody will ever page past; an empty host is a cursor
	// from the flat list, and accepting it would silently restart the walk.
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Host == "" {
		return GroupCursor{}, refuse("the cursor is not one this list handed out")
	}
	return cursor, nil
}

// Zero reports whether a walk is starting.
func (c GroupCursor) Zero() bool { return c.Host == "" }

// GroupRequest is one question about an inventory, folded.
type GroupRequest struct {
	Filter Node
	Limit  int
	Cursor GroupCursor
}

// hostRow is one host of the page, with the timestamp it is ordered by.
type hostRow struct {
	host     string
	lastSeen time.Time
}

// ListGrouped answers one page of hosts.
//
// Two statements rather than one. The first picks the page's hosts, ordered by
// their most recent asset; the second reads every asset of those hosts under
// the same filter. A single statement with a window function would read the
// whole filtered set to number it, which on a filter matching a hundred
// thousand assets is the expensive half done for fifty rows.
func ListGrouped(ctx context.Context, q Querier, org uuid.UUID, req GroupRequest) (GroupedPage, error) {
	compiled, err := Compile(org, req.Filter)
	if err != nil {
		return GroupedPage{}, err
	}

	limit := req.Limit
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		return GroupedPage{}, refuse("a page of %d hosts exceeds the bound of %d", limit, MaxLimit)
	}

	page := GroupedPage{Groups: []Group{}}

	// One host beyond the page is how the next cursor is known to exist,
	// without a second count over the same predicate.
	hosts, err := hostsOf(ctx, q, compiled, limit+1, req.Cursor)
	if err != nil {
		return GroupedPage{}, err
	}
	if len(hosts) > limit {
		hosts = hosts[:limit]
		last := hosts[limit-1]
		page.Next = GroupCursor{LastSeen: last.lastSeen, Host: last.host}.String()
	}
	if len(hosts) == 0 {
		return page, nil
	}

	rows, err := assetsOfHosts(ctx, q, compiled, hosts)
	if err != nil {
		return GroupedPage{}, err
	}
	page.Groups = fold(hosts, rows)

	images, err := Favicons(ctx, q, org, rows)
	if err != nil {
		return GroupedPage{}, err
	}
	page.Favicons = images
	return page, nil
}

// hostsOf picks the hosts of one page.
func hostsOf(
	ctx context.Context, q Querier, compiled Compiled, limit int, cursor GroupCursor,
) ([]hostRow, error) {
	args := append([]any{}, compiled.Args...)

	// The cursor bounds the group, in HAVING alone, and never the row.
	//
	// A row level bound looks like a free narrowing and is a defect. The
	// argument for it, that a host whose maximum is below the cursor has every
	// row below it, is true of the host and false of the aggregate: dropping
	// the rows above changes what max() is computed from, so a host already
	// returned comes back with a smaller maximum and passes the bound a second
	// time.
	//
	// The cost is stated rather than hidden: without a row level bound the
	// aggregate is computed over the whole filtered set on every page. On the
	// perimeters this is built for that is a grouping over thousands of rows,
	// and the honest fix the day it stops being is a materialized per host
	// timestamp, not a bound that is wrong.
	having := ""
	if !cursor.Zero() {
		args = append(args, cursor.LastSeen, cursor.Host)
		having = fmt.Sprintf(
			" HAVING (max(%[1]s.last_seen), COALESCE(%[1]s.host, %[1]s.key)) < ($%[2]d, $%[3]d)",
			Alias, len(args)-1, len(args))
	}
	args = append(args, limit)

	sql := fmt.Sprintf(
		`SELECT COALESCE(%[1]s.host, %[1]s.key) AS host, max(%[1]s.last_seen) AS last_seen
		   FROM asset_current %[1]s
		  WHERE %[2]s
		  GROUP BY 1%[3]s
		  ORDER BY last_seen DESC, host DESC
		  LIMIT $%[4]d`, Alias, compiled.SQL, having, len(args))

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("run the host list: %w", err)
	}
	defer rows.Close()

	out := make([]hostRow, 0, limit)
	for rows.Next() {
		var row hostRow
		if err := rows.Scan(&row.host, &row.lastSeen); err != nil {
			return nil, fmt.Errorf("read a host: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the host list: %w", err)
	}
	return out, nil
}

// assetsOfHosts reads every asset of the page's hosts, under the same filter.
//
// Under the same filter, which is what makes the counts true: a group on a list
// filtered to status_code = 200 shows the services that match and not the eight
// the host has. Reading every asset of the host instead would make the fold
// disagree with the facets beside it.
func assetsOfHosts(ctx context.Context, q Querier, compiled Compiled, hosts []hostRow) ([]Row, error) {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.host)
	}

	args := append([]any{}, compiled.Args...)
	// The list is a display, always. There is no grouped export: the export
	// walks the flat list, so nothing here ever needs the badges off.
	args = append(args, true)
	join := fmt.Sprintf(pivotJoin, len(args))
	args = append(args, names)

	sql := fmt.Sprintf("SELECT"+selectColumns+
		" FROM asset_current %[1]s"+lineageJoin+join+
		" WHERE %[2]s AND COALESCE(%[1]s.host, %[1]s.key) = ANY($%[3]d::text[])"+
		" ORDER BY %[1]s.last_seen DESC, %[1]s.asset_id", Alias, compiled.SQL, len(args))

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("run the grouped list: %w", err)
	}
	defer rows.Close()

	out := make([]Row, 0, len(hosts)*2)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the grouped list: %w", err)
	}
	return out, nil
}

// fold puts the assets into their groups, keeping the order of the hosts.
//
// In Go rather than in the query, because the order of the groups is decided by
// the first statement and the order inside a group by the second. Expressing
// both in one ORDER BY would mean sorting the assets by their host's timestamp,
// which is a column the row does not carry.
func fold(hosts []hostRow, rows []Row) []Group {
	byHost := make(map[string][]Row, len(hosts))
	for _, row := range rows {
		host := row.Key
		if row.Host != nil && *row.Host != "" {
			host = *row.Host
		}
		byHost[host] = append(byHost[host], row)
	}

	groups := make([]Group, 0, len(hosts))
	for _, host := range hosts {
		members := byHost[host.host]
		if len(members) == 0 {
			// A host whose assets vanished between the two statements. Rare and
			// not an error: an empty group is dropped rather than rendered as a
			// header over nothing.
			continue
		}
		groups = append(groups, Group{Host: host.host, LastSeen: host.lastSeen, Rows: members})
	}
	return groups
}

// Favicons reads the images the rows of a page refer to.
//
// Rendered as data URIs. An endpoint per image is a request per row from a page
// that has just been drawn, and the bound that makes this safe is already in
// the schema: an image above 64 kB is not stored, so a page of fifty carries
// what a page of fifty can carry.
func Favicons(ctx context.Context, q Querier, org uuid.UUID, rows []Row) (map[string]string, error) {
	seen := map[string]struct{}{}
	hashes := make([]string, 0, 8)
	for _, row := range rows {
		hash, ok := row.Attributes["favicon_hash"].(string)
		if !ok || hash == "" {
			continue
		}
		if _, held := seen[hash]; held {
			continue
		}
		seen[hash] = struct{}{}
		hashes = append(hashes, hash)
	}
	if len(hashes) == 0 {
		return nil, nil
	}

	// The organization is in the statement as well as in the policy. The two
	// are not redundant: one is structural and the other is the guarantee this
	// query cannot remove from itself.
	images, err := q.Query(ctx, `
		SELECT hash, media_type, bytes
		  FROM favicon_image
		 WHERE org_id = $1 AND hash = ANY($2::text[])`, org, hashes)
	if err != nil {
		return nil, fmt.Errorf("run the favicon read: %w", err)
	}
	defer images.Close()

	out := make(map[string]string, len(hashes))
	for images.Next() {
		var hash, mediaType string
		var bytes []byte
		if err := images.Scan(&hash, &mediaType, &bytes); err != nil {
			return nil, fmt.Errorf("read a favicon: %w", err)
		}
		out[hash] = "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(bytes)
	}
	if err := images.Err(); err != nil {
		return nil, fmt.Errorf("read the favicons: %w", err)
	}
	// A hash with no image is left out rather than mapped to an empty string. A
	// favicon above the bound is not stored, and a page that received "" for it
	// would render a broken image where the honest answer is no image.
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
