package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChangeKinds are the kinds of change, as the change table writes them.
var ChangeKinds = []string{
	"new_asset", "lifecycle_changed", "scope_changed", "dns_changed", "port_opened", "port_closed",
	"http_changed", "tech_added", "tech_removed", "tech_version_changed", "render_changed",
}

// changeField is the feed's own field: the kind of the change itself.
var changeField = Field{
	Name: "change", Kind: KEnum, column: "ch.kind", Values: ChangeKinds,
	Doc: "the kind of change, in the changes feed", Example: "change:port_opened,tech_version_changed",
}

// FeedRequest is a read of the changes feed.
type FeedRequest struct {
	Query string
	// Unseen keeps the changes newer than their asset's seen_at, ignored
	// assets left out.
	Unseen bool
	Run    *uuid.UUID
	Cursor string
	Limit  int
	Now    time.Time
}

// FeedRow is one change and the asset it happened to.
type FeedRow struct {
	ID        int64           `json:"id"`
	AssetID   uuid.UUID       `json:"asset_id"`
	ProgramID uuid.UUID       `json:"program_id"`
	Program   string          `json:"program"`
	AssetKind string          `json:"asset_kind"`
	AssetKey  string          `json:"asset_key"`
	Kind      string          `json:"kind"`
	Layer     *string         `json:"layer"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	RunID     *uuid.UUID      `json:"run_id"`
	At        time.Time       `json:"at"`
	Unseen    bool            `json:"unseen"`
}

// FeedPage is a page of the feed.
type FeedPage struct {
	Query   string    `json:"query"`
	Changes []FeedRow `json:"changes"`
	Next    string    `json:"next,omitempty"`
}

const unseen = `ch.at > COALESCE(tr.seen_at, '-infinity') AND COALESCE(tr.state, 'new') <> 'ignored'`

// feedWhere is the condition of a feed read, its args numbered from $1.
func feedWhere(req FeedRequest) (Node, Compiled, error) {
	n, err := Parse(req.Query)
	if err != nil {
		return nil, Compiled{}, err
	}
	c, err := compile(n, req.Now, true)
	if err != nil {
		return nil, Compiled{}, err
	}
	if req.Unseen {
		c.Where = "(" + c.Where + ") AND " + unseen
	}
	if req.Run != nil {
		c.Args = append(c.Args, *req.Run)
		c.Where = "(" + c.Where + ") AND ch.run_id = $" + strconv.Itoa(len(c.Args))
	}
	return n, c, nil
}

const feedFrom = From + `
JOIN change ch ON ch.asset_id = ac.asset_id`

// Feed reads a page of changes, newest first.
func Feed(ctx context.Context, db Querier, req FeedRequest) (FeedPage, error) {
	req.Limit = min(max(req.Limit, 1), 500)
	n, c, err := feedWhere(req)
	if err != nil {
		return FeedPage{}, err
	}
	page := FeedPage{Changes: []FeedRow{}}
	if n != nil {
		page.Query = n.String()
	}
	args := c.Args
	where := c.Where
	if req.Cursor != "" {
		at, id, ok := decodeFeedCursor(req.Cursor)
		if !ok {
			return FeedPage{}, ErrBadCursor
		}
		args = append(args, at, id)
		where += " AND (ch.at, ch.id) < ($" + strconv.Itoa(len(args)-1) + ", $" + strconv.Itoa(len(args)) + ")"
	}
	args = append(args, req.Limit+1)
	rows, err := db.Query(ctx, `SELECT ch.id, ac.asset_id, ac.program_id, p.name, ac.kind, ac.key, ch.kind, ch.layer,
  ch.before, ch.after, ch.run_id, ch.at, `+unseen+`
FROM `+feedFrom+` WHERE `+where+` ORDER BY ch.at DESC, ch.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return FeedPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r FeedRow
		if err := rows.Scan(&r.ID, &r.AssetID, &r.ProgramID, &r.Program, &r.AssetKind, &r.AssetKey, &r.Kind, &r.Layer,
			&r.Before, &r.After, &r.RunID, &r.At, &r.Unseen); err != nil {
			return FeedPage{}, err
		}
		page.Changes = append(page.Changes, r)
	}
	if err := rows.Err(); err != nil {
		return FeedPage{}, err
	}
	if len(page.Changes) > req.Limit {
		page.Changes = page.Changes[:req.Limit]
		last := page.Changes[len(page.Changes)-1]
		page.Next = encodeFeedCursor(last.At, last.ID)
	}
	return page, nil
}

// MaxUnseen is where the unseen count stops counting: past it, the navigation
// says "1000+".
const MaxUnseen = 1000

// UnseenCount counts the unseen changes, up to MaxUnseen.
func UnseenCount(ctx context.Context, db Querier) (int, error) {
	rows, err := db.Query(ctx, `SELECT count(*)::int FROM (SELECT 1 FROM `+feedFrom+` WHERE `+unseen+` LIMIT $1) u`, MaxUnseen)
	if err != nil {
		return 0, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowTo[int])
}

// MarkSeen marks as seen the assets behind the unseen changes a read shows, as
// opening each would, and returns how many assets that was.
func MarkSeen(ctx context.Context, db Querier, req FeedRequest) (int, error) {
	req.Unseen = true
	_, c, err := feedWhere(req)
	if err != nil {
		return 0, err
	}
	args := append(c.Args, req.Now)
	now := "$" + strconv.Itoa(len(args))
	rows, err := db.Query(ctx, `WITH t AS (SELECT DISTINCT ac.asset_id FROM `+feedFrom+` WHERE `+c.Where+`)
INSERT INTO triage (asset_id, state, seen_at) SELECT asset_id, 'seen', `+now+`::timestamptz FROM t
ON CONFLICT (asset_id) DO UPDATE SET seen_at = EXCLUDED.seen_at,
  state = CASE WHEN triage.state = 'new' THEN 'seen' ELSE triage.state END
RETURNING asset_id`, args...)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return len(ids), err
}

func encodeFeedCursor(at time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("feed|" + at.Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)))
}

func decodeFeedCursor(s string) (time.Time, int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, false
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 3 || parts[0] != "feed" {
		return time.Time{}, 0, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	return at, id, err == nil
}
