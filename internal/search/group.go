package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Grouping the list by host. Every open port is an asset, so one name occupies several rows
// sharing ASN, geolocation and often certificate. The fold must happen here, not in the page:
// folding fifty fetched assets breaks at the page boundary, rendering a host split across
// pages as two partial groups with wrong counts. So pagination is over hosts, with a host
// cursor.

// Group is one host and the assets of the result that belong to it.
type Group struct {
	// Host is the value the members share. A row with a null host is grouped under
	// its own key, alone and in place, instead of pooling all of them under a
	// meaningless name.
	Host string `json:"host"`
	// LastSeen is the most recent of the group and what groups are ordered by, so
	// folding keeps recently moved hosts on top.
	LastSeen time.Time `json:"last_seen"`
	Rows     []Row     `json:"assets"`
}

// GroupedPage is one answer of the folded list.
type GroupedPage struct {
	Groups []Group `json:"groups"`
	// Favicons are the page's images keyed by hash. On the page, not each row: a
	// shared favicon is the interesting case, and repeating two kilobytes per asset
	// would undo storing one copy.
	Favicons map[string]string `json:"favicons,omitempty"`
	Next     string            `json:"next_cursor,omitempty"`
}

// GroupCursor is where a walk over hosts left off. It carries the host, not an
// asset id: two hosts can share a last_seen to the microsecond on a first scan,
// and the tie needs a stable break.
type GroupCursor struct {
	LastSeen time.Time `json:"s"`
	Host     string    `json:"h"`
}

// String encodes it for a client to hand back. Opaque, and deliberately unlike
// the flat list's encoding: swapping them is refused, instead of restarting or
// skipping.
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
	// The host must be there. A zero timestamp is legitimate; an empty host is a
	// flat-list cursor, and accepting it would silently restart the walk.
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

// ListGrouped answers one page of hosts in two statements: one picks the page's
// hosts by most recent asset, the other reads every asset of those hosts under
// the same filter. A window function would read the whole filtered set to
// number it, the expensive half for fifty rows.
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

	// One host beyond the page tells a next cursor exists, without a second count.
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

	// The cursor bounds the group in HAVING alone, never the row: dropping the rows above the
	// cursor changes what max() is computed from, so a host already returned comes back with a
	// smaller maximum and passes the bound again. The cost is stated: the aggregate runs over the
	// whole filtered set on every page (thousands of rows at the intended scale); if it stops
	// being so, the fix is a materialized per-host timestamp, not a wrong bound.
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

// assetsOfHosts reads every asset of the page's hosts under the same filter,
// which makes the counts true: a group on a list filtered to status_code = 200
// shows the matching services, not all eight, and agrees with the facets.
func assetsOfHosts(ctx context.Context, q Querier, compiled Compiled, hosts []hostRow) ([]Row, error) {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.host)
	}

	args := append([]any{}, compiled.Args...)
	// The list is always a display. There is no grouped export, so the badges are
	// never off here.
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

// fold puts the assets into their groups, keeping the hosts' order. In Go
// because the first statement decides group order and the second decides order
// inside a group; one ORDER BY would need the host's timestamp, which the row
// lacks.
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
			// A host whose assets vanished between the statements: dropped, not rendered as
			// a header over nothing.
			continue
		}
		groups = append(groups, Group{Host: host.host, LastSeen: host.lastSeen, Rows: members})
	}
	return groups
}

// Favicons reads the images the rows of a page refer to, as data URIs: an
// endpoint per image would be a request per row. The schema already bounds it,
// since an image above 64 kB is not stored.
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

	return faviconsByHash(ctx, q, org, hashes)
}

// faviconsByHash reads the images a set of hashes names. Separate because the
// callers differ: a page has rows, the sidebar has the values its aggregation
// ranked.
func faviconsByHash(ctx context.Context, q Querier, org uuid.UUID, hashes []string) (map[string]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}

	// The organization is in the statement as well as the policy; one is
	// structural, the other the guarantee this query cannot remove from itself.
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
	// A hash with no image is left out, not mapped to "": an image above the bound
	// is not stored, and "" would render a broken image.
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
