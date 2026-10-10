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
// Polling with a cursor, not LISTEN/NOTIFY. Discovery already arrives in
// batches (a run posts one report), so sub-second latency describes nothing
// real. LISTEN would also add a NOTIFY round trip to the hottest write path,
// NOTIFY is global so a subscriber would receive every organization's channels,
// and LISTEN pins a pool connection per open tab.

// FeedCap is how many discoveries one round emits. A program's first run
// produces thousands, and emitting them at arrival rate makes the tab unusable
// when there is most to see.
const FeedCap = 50

// FeedLag is how far behind the present the feed reads. The cursor orders on first_seen,
// stamped by the application inside the ingestion transaction, not at commit: a run stamping
// T1 that takes ten seconds to commit lands after one stamping T2 > T1, so a tick in between
// advances the cursor past T2 and never sees the first, silently. The feed therefore reads up
// to now minus this, which must exceed the longest ingestion transaction; the cost is latency,
// the cheapest thing to spend here.
const FeedLag = 30 * time.Second

// FeedOverflowCap bounds the count of what a round left behind. The count scans
// the index tail: cheap on a normal tick, unbounded on a first import of half a
// million assets. Capped, the tick says "500 or more".
const FeedOverflowCap = 500

// Discovery is one line of the feed: what appeared, and why. Enough for a row;
// more would put the asset view's cost on a loop running every few seconds.
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
	// Step is the last lineage step, the one that produced this asset. The whole
	// chain belongs on the asset view.
	Step *string `json:"step,omitempty"`
}

// Tick is one round of the feed, as a batch. One message per asset would put the
// cap in the wrong place: four hundred messages of which the client keeps fifty,
// and no message to carry the overflow.
type Tick struct {
	Discoveries []Discovery `json:"discoveries"`
	// Overflow is what the cap left out.
	Overflow int `json:"overflow,omitempty"`
	// OverflowAtLeast says the count itself was capped, so the number is a floor.
	OverflowAtLeast bool `json:"overflow_at_least,omitempty"`
	// Cursor is the message id. The client hands it back as Last-Event-ID, so
	// resuming needs no server-side state.
	Cursor string `json:"cursor"`
}

// FeedCursor is where the walk left off.
type FeedCursor struct {
	FirstSeen time.Time
	AssetID   uuid.UUID
}

// Zero reports whether a walk has no position yet.
func (c FeedCursor) Zero() bool { return c.AssetID == uuid.Nil }

// String encodes it as an event id: one line without whitespace (SSE framing),
// and opaque for the same reason as the list's cursor.
func (c FeedCursor) String() string {
	if c.Zero() {
		return ""
	}
	return "f." + base64.RawURLEncoding.EncodeToString(
		[]byte(c.FirstSeen.UTC().Format(time.RFC3339Nano)+" "+c.AssetID.String()))
}

// ParseFeedCursor reads one back. The prefix stops a list cursor being accepted
// here: they encode the same pair for different columns, and mixing them would
// start the walk in the wrong place silently.
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

// Discoveries reads one round. Ascending, keeping the oldest, so the cursor
// advances; keeping the newest would re-read the same head and stall on a first
// run. It reads asset and asset_current like the list, never observation.
func Discoveries(
	ctx context.Context, q Querier, org uuid.UUID, cursor FeedCursor, now time.Time,
) (Tick, error) {
	tick := Tick{Discoveries: []Discovery{}, Cursor: cursor.String()}
	// The watermark; see FeedLag.
	upTo := now.Add(-FeedLag)

	rows, err := q.Query(ctx, `
		SELECT c.asset_id, c.program_id, c.kind, c.key, c.host,
		       c.lifecycle, c.scope_status, c.first_seen,
		       a.discovery_source, a.discovery_path
		  FROM asset_current c
		  JOIN asset a ON a.id = c.asset_id
		 WHERE c.org_id = $1
		   AND (c.first_seen > $2 OR (c.first_seen = $2 AND c.asset_id > $3))
		   AND c.first_seen <= $4
		 ORDER BY c.first_seen, c.asset_id
		 LIMIT $5`,
		org, cursor.FirstSeen, cursor.AssetID, upTo, FeedCap)
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
		// A round that found nothing emits nothing and the cursor stays: advancing it
		// would hand out an id that never named a discovery.
		return tick, nil
	}

	last := tick.Discoveries[len(tick.Discoveries)-1]
	next := FeedCursor{FirstSeen: last.FirstSeen, AssetID: last.AssetID}
	tick.Cursor = next.String()

	if len(tick.Discoveries) == FeedCap {
		overflow, capped, err := backlog(ctx, q, org, next, upTo)
		if err != nil {
			return Tick{}, err
		}
		tick.Overflow, tick.OverflowAtLeast = overflow, capped
	}
	return tick, nil
}

// backlog counts what a full round left behind, bounded. Under the same
// watermark as the round, so it describes what the next tick will deliver.
func backlog(
	ctx context.Context, q Querier, org uuid.UUID, after FeedCursor, upTo time.Time,
) (int, bool, error) {
	rows, err := q.Query(ctx, `
		SELECT count(*) FROM (
		    SELECT 1
		      FROM asset_current c
		     WHERE c.org_id = $1
		       AND (c.first_seen > $2 OR (c.first_seen = $2 AND c.asset_id > $3))
		       AND c.first_seen <= $4
		     LIMIT $5
		) waiting`, org, after.FirstSeen, after.AssetID, upTo, FeedOverflowCap)
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

// Head is where a feed with no cursor starts: the present. A tab opening on a
// feed asks what is happening now; replaying the inventory would repeat the list.
func Head(now time.Time) FeedCursor {
	// The largest possible id, so an asset landing in the connection's microsecond
	// falls below the bound instead of arriving as a discovery predating the tab.
	return FeedCursor{FirstSeen: now, AssetID: uuid.Max}
}

// lastStep reads the step that produced an asset from its lineage: a chain of
// objects, newest last, each with a step. Only the name is read; the rest is the
// asset view's. Anything else leaves the field absent, since the chain is
// written by whatever discovered the asset and this reads someone else's shape.
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
