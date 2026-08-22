package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The live feed of discoveries.
//
// Polling with a cursor rather than LISTEN/NOTIFY, and the argument that
// decides is not cost but the nature of the data: discovery already arrives in
// batches. A run posts one report, so sub second latency describes nothing
// real, and paying a notification per asset to show a batch faster than it
// exists would be optimizing against the producer.
//
// What LISTEN would have cost, written down because it is the option that looks
// modern: a NOTIFY in the ingestion transaction is another round trip on the
// hottest write path, NOTIFY is global so a subscriber receives every
// organization's channels, and LISTEN pins a connection, so an open tab costs a
// pool connection.

// FeedCap is how many discoveries one round emits.
//
// The first run of a program produces thousands of assets, and a feed emitting
// all of them at the rate they arrive makes the tab unusable at the exact
// moment there is most to see.
const FeedCap = 50

// FeedOverflowCap bounds the count of what a round left behind.
//
// The count is over the tail of the same index, so it is cheap on an ordinary
// tick and unbounded on a first import of half a million assets. Bounded, the
// tick says "500 or more" instead of spending two seconds computing a number
// nobody reads differently past the first hundred.
const FeedOverflowCap = 500

// Discovery is one line of the feed: what appeared, and why.
//
// Enough for a row, not a card. The rest is one click away, and reading more
// here would put the asset view's cost on a loop that runs every few seconds.
type Discovery struct {
	AssetID     uuid.UUID `json:"asset_id"`
	ProgramID   uuid.UUID `json:"program_id"`
	Kind        string    `json:"kind"`
	Key         string    `json:"key"`
	Host        *string   `json:"host,omitempty"`
	Lifecycle   string    `json:"lifecycle"`
	ScopeStatus string    `json:"scope_status"`
	FirstSeen   time.Time `json:"first_seen"`
	Source      string    `json:"discovery_source"`
	// Step is the last step of the lineage, which is the one that produced this
	// asset. The whole chain belongs on the asset view; here it answers "why"
	// in one line.
	Step *string `json:"step,omitempty"`
}

// Tick is one round of the feed.
//
// A batch rather than one message per discovery. One message per asset would
// put the cap in the wrong place: a round that found four hundred assets would
// emit four hundred messages of which the client keeps the last fifty, and the
// overflow the round is supposed to announce would have no message to travel
// in.
type Tick struct {
	Discoveries []Discovery `json:"discoveries"`
	// Overflow is what the cap left out, answered rather than dropped.
	Overflow int `json:"overflow,omitempty"`
	// OverflowAtLeast says the count itself was capped, so the number is a
	// floor. A page rendering "500 more" where there are forty thousand is the
	// silent truncation this whole document refuses.
	OverflowAtLeast bool `json:"overflow_at_least,omitempty"`
	// Cursor is the id of the message. The client hands it back as
	// Last-Event-ID on reconnection, so resumption costs no server side state.
	Cursor string `json:"cursor"`
}

// FeedCursor is where the walk left off.
type FeedCursor struct {
	FirstSeen time.Time
	AssetID   uuid.UUID
}

// Zero reports whether a walk has no position yet.
func (c FeedCursor) Zero() bool { return c.AssetID == uuid.Nil }

// String encodes it as an event id.
//
// One line, no whitespace, which the SSE framing needs, and opaque for the same
// reason the list's cursor is: a cursor a client can build is one a client will
// build by hand, and then the ordering key cannot change.
func (c FeedCursor) String() string {
	if c.Zero() {
		return ""
	}
	return "f." + base64.RawURLEncoding.EncodeToString(
		[]byte(c.FirstSeen.UTC().Format(time.RFC3339Nano)+" "+c.AssetID.String()))
}

// ParseFeedCursor reads one back.
//
// The prefix is what stops a list cursor from being accepted here. The two
// encode the same pair and mean different columns, so without it a client that
// mixed them would get a walk that silently starts in the wrong place.
func ParseFeedCursor(encoded string) (FeedCursor, error) {
	if encoded == "" {
		return FeedCursor{}, nil
	}
	body, found := strings.CutPrefix(encoded, "f.")
	if !found {
		return FeedCursor{}, refuse("the cursor is not one this feed handed out")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return FeedCursor{}, refuse("the cursor is not one this feed handed out")
	}
	instant, id, split := strings.Cut(string(raw), " ")
	if !split {
		return FeedCursor{}, refuse("the cursor is not one this feed handed out")
	}
	at, err := time.Parse(time.RFC3339Nano, instant)
	if err != nil {
		return FeedCursor{}, refuse("the cursor is not one this feed handed out")
	}
	asset, err := uuid.Parse(id)
	if err != nil {
		return FeedCursor{}, refuse("the cursor is not one this feed handed out")
	}
	return FeedCursor{FirstSeen: at, AssetID: asset}, nil
}

// Discoveries reads one round.
//
// Ascending and keeping the oldest, which is what makes the cursor advance past
// them. Keeping the newest would leave the cursor where it was and re-read the
// same head of the queue on the next tick, and the feed would stall on a first
// run rather than draining it.
//
// No query on observation here either. It reads asset and asset_current, like
// the list.
func Discoveries(ctx context.Context, q Querier, org uuid.UUID, cursor FeedCursor) (Tick, error) {
	tick := Tick{Discoveries: []Discovery{}, Cursor: cursor.String()}

	rows, err := q.Query(ctx, `
		SELECT c.asset_id, c.program_id, c.kind, c.key, c.host,
		       c.lifecycle, c.scope_status, c.first_seen,
		       a.discovery_source, a.discovery_path
		  FROM asset_current c
		  JOIN asset a ON a.id = c.asset_id
		 WHERE c.org_id = $1
		   AND (c.first_seen > $2 OR (c.first_seen = $2 AND c.asset_id > $3))
		 ORDER BY c.first_seen, c.asset_id
		 LIMIT $4`,
		org, cursor.FirstSeen, cursor.AssetID, FeedCap)
	if err != nil {
		return Tick{}, fmt.Errorf("run the feed: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row Discovery
		var lineage []byte
		if err := rows.Scan(&row.AssetID, &row.ProgramID, &row.Kind, &row.Key, &row.Host,
			&row.Lifecycle, &row.ScopeStatus, &row.FirstSeen,
			&row.Source, &lineage); err != nil {
			return Tick{}, fmt.Errorf("read a discovery: %w", err)
		}
		row.Step = lastStep(lineage)
		tick.Discoveries = append(tick.Discoveries, row)
	}
	if err := rows.Err(); err != nil {
		return Tick{}, fmt.Errorf("read the feed: %w", err)
	}

	if len(tick.Discoveries) == 0 {
		// A round that found nothing emits nothing, and the cursor stays where
		// it was. Advancing it on an empty round would hand out an id that
		// never named a discovery, and a client resuming from it would be
		// resuming from a position the server invented.
		return tick, nil
	}

	last := tick.Discoveries[len(tick.Discoveries)-1]
	next := FeedCursor{FirstSeen: last.FirstSeen, AssetID: last.AssetID}
	tick.Cursor = next.String()

	if len(tick.Discoveries) == FeedCap {
		overflow, capped, err := backlog(ctx, q, org, next)
		if err != nil {
			return Tick{}, err
		}
		tick.Overflow, tick.OverflowAtLeast = overflow, capped
	}
	return tick, nil
}

// backlog counts what a full round left behind, bounded.
func backlog(ctx context.Context, q Querier, org uuid.UUID, after FeedCursor) (int, bool, error) {
	rows, err := q.Query(ctx, `
		SELECT count(*) FROM (
		    SELECT 1
		      FROM asset_current c
		     WHERE c.org_id = $1
		       AND (c.first_seen > $2 OR (c.first_seen = $2 AND c.asset_id > $3))
		     LIMIT $4
		) waiting`, org, after.FirstSeen, after.AssetID, FeedOverflowCap)
	if err != nil {
		return 0, false, fmt.Errorf("run the feed backlog: %w", err)
	}
	defer rows.Close()

	count := 0
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return 0, false, fmt.Errorf("read the feed backlog: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("read the feed backlog: %w", err)
	}
	return count, count >= FeedOverflowCap, nil
}

// Head is where a feed with no cursor starts.
//
// At the present rather than at the beginning of the inventory. A tab opening
// on a feed is asking what is happening now, and starting from the first asset
// ever recorded would spend the first minutes of every connection replaying an
// inventory the list already shows.
func Head(now time.Time) FeedCursor {
	// The identifier is the largest possible one, so an asset landing in the
	// same microsecond as the connection opens falls below the bound rather
	// than arriving as a discovery that predates the tab.
	return FeedCursor{FirstSeen: now, AssetID: uuid.Max}
}

// lastStep reads the step that produced an asset out of its lineage.
//
// The lineage is a chain of objects, newest last, and each one carries a step
// beside whatever else the producer recorded. Only the name is read here: the
// rest is the asset view's, which has room for it.
//
// Anything the chain holds that is not one of those leaves the field absent
// rather than rendering a structure into a row. The lineage is written by
// whatever discovered the asset, so this is a read of somebody else's shape.
func lastStep(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	var path []any
	if err := json.Unmarshal(raw, &path); err != nil || len(path) == 0 {
		return nil
	}
	switch last := path[len(path)-1].(type) {
	case map[string]any:
		step, ok := last["step"].(string)
		if !ok || step == "" {
			return nil
		}
		return &step
	case string:
		if last == "" {
			return nil
		}
		return &last
	default:
		return nil
	}
}
