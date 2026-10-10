//go:build integration

// Phase 7's read half: the folded list, the asset view and the live feed,
// measured against a database rather than against a string.
package search_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/search"
)

// A host is a header and its services are rows, folded server-side. Folding a
// fetched page breaks at the page boundary: a host split across pages renders as two
// partial groups with wrong counts.
func TestTheListFoldsByHostAcrossPages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Three hosts, one of them carrying four rows. With a page of two hosts,
	// the four have to arrive together or not at all.
	base := time.Now().UTC().Add(-time.Hour)
	for i, key := range []string{
		"many.target.test", "many.target.test:443/tcp", "many.target.test:8080/tcp",
		"many.target.test:8443/tcp",
	} {
		h.asset(t, h.org, key, map[string]any{
			"last_seen": base.Add(time.Duration(i) * time.Minute), "port": port(key),
		})
	}
	h.asset(t, h.org, "one.target.test", map[string]any{"last_seen": base.Add(2 * time.Hour)})
	h.asset(t, h.org, "two.target.test", map[string]any{"last_seen": base.Add(time.Hour)})

	var first search.GroupedPage
	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{
			Filter: filter(t, `{"op":"and"}`), Limit: 2,
		})
		if err != nil {
			t.Fatalf("grouped list: %v", err)
		}
		first = page
	})

	if len(first.Groups) != 2 {
		t.Fatalf("groups = %d, want the page of 2 that was asked for", len(first.Groups))
	}
	// Ordered by their most recent asset, so what moved recently stays at the
	// top one level up.
	if first.Groups[0].Host != "one.target.test" || first.Groups[1].Host != "two.target.test" {
		t.Errorf("hosts = %s, %s, want them ordered by their most recent asset",
			first.Groups[0].Host, first.Groups[1].Host)
	}
	if first.Next == "" {
		t.Fatal("no cursor on a page that left a host behind")
	}

	var second search.GroupedPage
	h.scoped(t, h.org, func(tx pgx.Tx) {
		cursor, err := search.ParseGroupCursor(first.Next)
		if err != nil {
			t.Fatalf("cursor: %v", err)
		}
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{
			Filter: filter(t, `{"op":"and"}`), Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		second = page
	})

	if len(second.Groups) != 1 {
		t.Fatalf("second page groups = %d, want the one host left", len(second.Groups))
	}
	// The whole point: the four rows of that host arrive together, on one page,
	// under one header.
	if got := len(second.Groups[0].Rows); got != 4 {
		t.Errorf("the four services of %s came back as %d, so the fold broke at the page boundary",
			second.Groups[0].Host, got)
	}
	if second.Next != "" {
		t.Errorf("a cursor on the last page: %s", second.Next)
	}
}

// The cursor bounds the group, never the row. A row bound looks like a free
// narrowing but changes what max() is computed from, so the host returns with a
// smaller maximum and passes the bound again.
func TestAHostIsNotReturnedTwice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	// The catching shape: a host whose newest asset puts it on page one and whose older
	// assets would still satisfy a row-level bound from that page's cursor.
	h.asset(t, h.org, "spread.target.test:443/tcp", map[string]any{"last_seen": base.Add(50 * time.Minute)})
	h.asset(t, h.org, "spread.target.test:80/tcp", map[string]any{"last_seen": base})
	h.asset(t, h.org, "later.target.test", map[string]any{"last_seen": base.Add(30 * time.Minute)})
	h.asset(t, h.org, "last.target.test", map[string]any{"last_seen": base.Add(10 * time.Minute)})

	seen := map[string]int{}
	cursor := search.GroupCursor{}
	for round := 0; round < 5; round++ {
		var page search.GroupedPage
		h.scoped(t, h.org, func(tx pgx.Tx) {
			got, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{
				Filter: filter(t, `{"op":"and"}`), Limit: 1, Cursor: cursor,
			})
			if err != nil {
				t.Fatalf("page %d: %v", round, err)
			}
			page = got
		})
		for _, group := range page.Groups {
			seen[group.Host]++
		}
		if page.Next == "" {
			break
		}
		parsed, err := search.ParseGroupCursor(page.Next)
		if err != nil {
			t.Fatalf("cursor: %v", err)
		}
		cursor = parsed
	}

	if len(seen) != 3 {
		t.Errorf("hosts = %v, want three distinct ones", seen)
	}
	for host, times := range seen {
		if times != 1 {
			t.Errorf("%s came back %d times, which is the row level bound rewriting max()", host, times)
		}
	}
}

// Both statements read under the same filter, making the counts true; reading every
// asset of the host would make the fold disagree with the facets.
func TestAGroupShowsOnlyWhatTheFilterMatched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.asset(t, h.org, "mixed.target.test:443/tcp", map[string]any{"port": 443, "status_code": 200})
	h.asset(t, h.org, "mixed.target.test:8080/tcp", map[string]any{"port": 8080, "status_code": 404})

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{
			Filter: filter(t, `{"field":"status_code","op":"eq","value":200}`),
		})
		if err != nil {
			t.Fatalf("grouped list: %v", err)
		}
		if len(page.Groups) != 1 {
			t.Fatalf("groups = %d, want 1", len(page.Groups))
		}
		if got := len(page.Groups[0].Rows); got != 1 {
			t.Errorf("the group shows %d services under a filter that matches one, so the "+
				"fold and the facets would disagree", got)
		}
	})
}

// Group and list cursors are not interchangeable: same pair, different columns, so
// swapping them must be refused and not silently restart the walk.
func TestTheTwoCursorsAreNotInterchangeable(t *testing.T) {
	flat := search.Cursor{LastSeen: time.Now(), AssetID: uuid.New()}.String()
	if _, err := search.ParseGroupCursor(flat); err == nil {
		t.Error("the grouped list accepted a flat cursor, which restarts the walk in silence")
	}

	grouped := search.GroupCursor{LastSeen: time.Now(), Host: "target.test"}.String()
	if _, err := search.ParseCursor(grouped); err == nil {
		t.Error("the flat list accepted a grouped cursor")
	}
	if _, err := search.ParseFeedCursor(flat); err == nil {
		t.Error("the feed accepted a list cursor, and the two order on different columns")
	}
}

// Favicon images travel once per page keyed by hash, not per asset: a shared favicon
// is the interesting case and the reason one copy is stored.
func TestTheFaviconsTravelOncePerPage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	attributes := []byte(`{"favicon_hash":"shared"}`)
	h.asset(t, h.org, "a.target.test", map[string]any{"attributes": attributes})
	h.asset(t, h.org, "b.target.test", map[string]any{"attributes": attributes})
	h.asset(t, h.org, "c.target.test", map[string]any{"attributes": []byte(`{"favicon_hash":"missing"}`)})
	h.exec(t, `INSERT INTO favicon_image (org_id, hash, media_type, bytes)
	           VALUES ($1, 'shared', 'image/png', '\x89504e47')`, h.org)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{Filter: filter(t, `{"op":"and"}`)})
		if err != nil {
			t.Fatalf("grouped list: %v", err)
		}
		if len(page.Favicons) != 1 {
			t.Fatalf("favicons = %v, want the one image two assets share", page.Favicons)
		}
		if !strings.HasPrefix(page.Favicons["shared"], "data:image/png;base64,") {
			t.Errorf("favicon = %q, want a data URI", page.Favicons["shared"])
		}
		// A hash whose image exceeds the bound is not stored, and is left out, not mapped to
		// "": that would render a broken image where the answer is no image.
		if _, present := page.Favicons["missing"]; present {
			t.Error("a hash with no stored image came back with a value")
		}
	})
}

// The favicon is faceted and the sidebar gets the images to draw it. It is the
// fastest identity signal an inventory has; without a facet, "which icons does this
// perimeter share" took one badge click at a time. An earlier facet list refused it,
// believing a key of `attributes` aggregates through the GIN index; the filter runs
// once into a CTE and every facet groups over that, costing what a column costs.
func TestTheFaviconIsFacetedWithItsImages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	shared := []byte(`{"favicon_hash":"shared"}`)
	h.asset(t, h.org, "one.target.test", map[string]any{"attributes": shared})
	h.asset(t, h.org, "two.target.test", map[string]any{"attributes": shared})
	h.asset(t, h.org, "three.target.test", map[string]any{"attributes": []byte(`{"favicon_hash":"lonely"}`)})
	h.exec(t, `INSERT INTO favicon_image (org_id, hash, media_type, bytes)
	           VALUES ($1, 'shared', 'image/png', '\x89504e47'), ($1, 'lonely', 'image/x-icon', '\x00000100')`,
		h.org)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.Facets(ctx, tx, h.org, filter(t, `{"op":"and"}`))
		if err != nil {
			t.Fatalf("facets: %v", err)
		}

		var favicons *search.Facet
		for i := range page.Facets {
			if page.Facets[i].Field == "favicon_hash" {
				favicons = &page.Facets[i]
			}
		}
		if favicons == nil {
			t.Fatal("no favicon facet, so the sidebar cannot ask which icons a perimeter shares")
		}
		// Ranked like any other facet: the shared one first, because that is the
		// one worth looking at.
		if len(favicons.Terms) != 2 || favicons.Terms[0].Value != "shared" || favicons.Terms[0].Count != 2 {
			t.Fatalf("terms = %+v, want the shared icon first with two assets", favicons.Terms)
		}

		// And the images come with it: from the page's rows, the most shared icon is often
		// one no row carries and would draw as a blank square with a count.
		for _, term := range favicons.Terms {
			if !strings.HasPrefix(page.Favicons[term.Value], "data:") {
				t.Errorf("the facet ranks %q and the sidebar got no image for it", term.Value)
			}
		}
	})
}

// The asset view is the one read path touching the journal, and each row is a change:
// the journal is deduplicated on write, so consecutive rows of one layer are distinct
// states.
func TestTheAssetViewReadsTheJournalAsChanges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.asset(t, h.org, "timeline.target.test:443/tcp", map[string]any{
		"port": 443, "status_code": 200, "title": "App",
	})
	base := time.Now().UTC().Add(-4 * time.Hour)
	h.observe(t, id, "http", base, base.Add(time.Hour), "1.0",
		`{"title":"Old","tech":["nginx"]}`)
	h.observe(t, id, "http", base.Add(2*time.Hour), base.Add(3*time.Hour), "1.0",
		`{"title":"App","tech":["nginx"]}`)
	h.observe(t, id, "dns", base, base.Add(4*time.Hour), "1.0", `{"addresses":["1.2.3.4"]}`)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		detail, err := search.Get(ctx, tx, h.org, id, time.Now())
		if err != nil {
			t.Fatalf("asset view: %v", err)
		}

		if detail.Asset.Key != "timeline.target.test:443/tcp" {
			t.Errorf("asset = %s, want the one that was asked for", detail.Asset.Key)
		}
		// The evidence is the last observation of each layer, whole.
		if len(detail.Evidence) != 2 {
			t.Fatalf("evidence = %d entries, want one per layer that has one", len(detail.Evidence))
		}
		if len(detail.Timeline) != 3 {
			t.Fatalf("timeline = %d entries, want one per journal row", len(detail.Timeline))
		}

		// The newest http entry carries the diff against the state before it,
		// and it is the Notifier's comparison rather than a second one.
		newest := detail.Timeline[0]
		if newest.Layer != "http" || newest.Diff == nil {
			t.Fatalf("the newest entry is %+v, want an http change with a diff", newest)
		}
		if newest.Diff.Class != search.ClassReal {
			t.Errorf("class = %s, want a real change: the http layer cannot claim a "+
				"detection improvement", newest.Diff.Class)
		}
		if len(newest.Diff.Fields) != 1 || newest.Diff.Fields[0].Field != "title" {
			t.Errorf("diff = %+v, want the title that moved", newest.Diff.Fields)
		}

		// The oldest entry read for a layer carries no diff, and that is not
		// "nothing changed": what it moved from is outside the window.
		oldest := detail.Timeline[len(detail.Timeline)-1]
		if oldest.Diff != nil {
			t.Errorf("the oldest entry carries a diff, so the screen cannot say "+
				"'not compared': %+v", oldest.Diff)
		}
		// Two sentences, not one. One is the last probe, the other is when the
		// state began, and side by side unnamed they read as stale data.
		if !oldest.HeldUntil.After(oldest.At) {
			t.Errorf("held_until %s is not after at %s", oldest.HeldUntil, oldest.At)
		}
	})
}

// The cap is stated on screen when it cuts, naming the layers: "the timeline was
// truncated" on a four-panel page does not say which to distrust.
func TestTheTimelineSaysWhichLayerItCut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.asset(t, h.org, "busy.target.test:443/tcp", map[string]any{"port": 443})
	// Inside the current month: observation is partitioned by month with no default
	// partition, so a row whose month is missing fails loudly.
	base := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -time.Now().UTC().Day()+1)
	for i := 0; i < search.TimelineCap+5; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		h.observe(t, id, "http", at, at.Add(30*time.Minute), "1.0",
			`{"status_code":`+itoa(200+i)+`}`)
	}
	h.observe(t, id, "dns", base, base.Add(time.Hour), "1.0", `{"addresses":["1.2.3.4"]}`)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		detail, err := search.Get(ctx, tx, h.org, id, time.Now())
		if err != nil {
			t.Fatalf("asset view: %v", err)
		}
		http := 0
		for _, entry := range detail.Timeline {
			if entry.Layer == "http" {
				http++
			}
		}
		if http != search.TimelineCap {
			t.Errorf("http entries = %d, want the cap of %d", http, search.TimelineCap)
		}
		if len(detail.Truncated) != 1 || detail.Truncated[0] != "http" {
			t.Errorf("truncated = %v, want http alone: dns was not cut and saying so "+
				"would make the whole page suspect", detail.Truncated)
		}
		// The last displayed entry of the cut layer still carries its diff: the row past the
		// cap is its comparison partner and is never shown, so the entry does not say "not
		// compared" with the previous state a row away.
		var last search.Change
		for _, entry := range detail.Timeline {
			if entry.Layer == "http" {
				last = entry
			}
		}
		if last.Diff == nil {
			t.Error("the oldest displayed http entry has no diff, and the state before " +
				"it is inside the window")
		}
	})
}

// An identifier from another organization answers the same absence as one that
// names nothing. The difference between the two enumerates what exists.
func TestAnotherTenantsAssetIsAnAbsence(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	theirs := h.asset(t, h.other, "secret.other.test", nil)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		if _, err := search.Get(ctx, tx, h.org, theirs, time.Now()); err == nil {
			t.Fatal("another tenant's asset was readable")
		} else if err != search.ErrNoAsset {
			t.Errorf("err = %v, want the same absence an unknown identifier gives", err)
		}
		if _, err := search.Get(ctx, tx, h.org, uuid.New(), time.Now()); err != search.ErrNoAsset {
			t.Errorf("err = %v on an unknown identifier, want the same absence", err)
		}
	})
}

// The feed re-emits nothing on an unchanged rescan, and says what a cap left
// out. An overflow never produces an absence of information.
func TestTheFeedDrainsWithoutRepeating(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	total := search.FeedCap + 7
	for i := 0; i < total; i++ {
		h.arrival(t, "host"+itoa(i)+".target.test", base.Add(time.Duration(i)*time.Second))
	}

	cursor := search.FeedCursor{}
	seen := map[string]int{}
	var firstTick search.Tick
	for round := 0; round < 4; round++ {
		var tick search.Tick
		h.scoped(t, h.org, func(tx pgx.Tx) {
			got, err := search.Discoveries(ctx, tx, h.org, cursor, time.Now())
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			tick = got
		})
		if round == 0 {
			firstTick = tick
		}
		if len(tick.Discoveries) == 0 {
			break
		}
		for _, discovery := range tick.Discoveries {
			seen[discovery.Key]++
		}
		parsed, err := search.ParseFeedCursor(tick.Cursor)
		if err != nil {
			t.Fatalf("cursor: %v", err)
		}
		cursor = parsed
	}

	if len(seen) != total {
		t.Errorf("the feed delivered %d distinct discoveries out of %d", len(seen), total)
	}
	for key, times := range seen {
		if times != 1 {
			t.Errorf("%s was emitted %d times", key, times)
		}
	}

	// The first round was full, so it says what it left behind rather than
	// staying silent about it.
	if len(firstTick.Discoveries) != search.FeedCap {
		t.Fatalf("first round emitted %d, want the cap of %d",
			len(firstTick.Discoveries), search.FeedCap)
	}
	if firstTick.Overflow != total-search.FeedCap {
		t.Errorf("overflow = %d, want %d", firstTick.Overflow, total-search.FeedCap)
	}
	if firstTick.OverflowAtLeast {
		t.Error("the count reported itself capped on a backlog well under the bound")
	}

	// And a rescan changes nothing. last_seen moves, first_seen does not, and
	// the feed orders on the one that cannot move forward.
	h.exec(t, `UPDATE asset_current SET last_seen = now() WHERE org_id = $1`, h.org)
	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, cursor, time.Now())
		if err != nil {
			t.Fatalf("after a rescan: %v", err)
		}
		if len(tick.Discoveries) != 0 {
			t.Errorf("a rescan re-emitted %d discoveries", len(tick.Discoveries))
		}
	})

	// A round that finds nothing leaves the cursor where it was. Advancing it
	// would hand out an id that never named a discovery.
	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, cursor, time.Now())
		if err != nil {
			t.Fatalf("empty round: %v", err)
		}
		if tick.Cursor != cursor.String() {
			t.Errorf("cursor moved to %s on an empty round, want %s", tick.Cursor, cursor.String())
		}
	})
}

// The feed reads behind a watermark, which stops it skipping. first_seen is stamped
// by the application inside the ingestion transaction, so stamp order differs from
// commit order: a run stamping early and committing late lands after one stamped
// later. A feed reading to the present emits the second, advances past it and never
// sees the first, invisibly.
func TestTheFeedDoesNotSkipASlowCommit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	now := time.Now().UTC()
	// Inside the lag: stamped, and by construction not yet safe to read.
	h.arrival(t, "slow.target.test", now.Add(-time.Second))
	// Outside it: old enough that any transaction which stamped it has landed.
	h.arrival(t, "settled.target.test", now.Add(-2*search.FeedLag))

	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, search.FeedCursor{}, now)
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		keys := make([]string, 0, len(tick.Discoveries))
		for _, discovery := range tick.Discoveries {
			keys = append(keys, discovery.Key)
		}
		if len(keys) != 1 || keys[0] != "settled.target.test" {
			t.Fatalf("tick carried %v, want only the one past the watermark: emitting the "+
				"recent one advances the cursor over anything still committing behind it", keys)
		}
	})

	// And it is a delay rather than a loss. The same asset arrives once the
	// watermark has passed it.
	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, search.FeedCursor{},
			now.Add(2*search.FeedLag))
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		if len(tick.Discoveries) != 2 {
			t.Errorf("later tick carried %d discoveries, want both: the watermark delays "+
				"and never drops", len(tick.Discoveries))
		}
	})
}

// A feed with no cursor starts at the present: a tab opening asks what is happening
// now, and replaying the inventory the list shows wastes the first minutes.
func TestAFreshFeedStartsAtThePresent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	opened := time.Now().UTC()
	h.arrival(t, "old.target.test", opened.Add(-time.Hour))
	head := search.Head(opened)

	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, head, opened)
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		if len(tick.Discoveries) != 0 {
			t.Errorf("a fresh feed replayed %d existing assets", len(tick.Discoveries))
		}
	})

	// After the connection opened and read once the watermark has moved past it. The lag
	// is what the second clock is for: an asset is emitted when every transaction that
	// could have stamped it has landed, not when it becomes visible.
	h.arrival(t, "new.target.test", opened.Add(time.Second))
	later := opened.Add(2 * search.FeedLag)
	h.scoped(t, h.org, func(tx pgx.Tx) {
		tick, err := search.Discoveries(ctx, tx, h.org, head, later)
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		if len(tick.Discoveries) != 1 || tick.Discoveries[0].Key != "new.target.test" {
			t.Fatalf("tick = %+v, want the one asset that arrived after the connection", tick.Discoveries)
		}
		// Enough for a row, not a card: what appeared and why.
		if tick.Discoveries[0].Step == nil {
			t.Error("no lineage step, which is the half of the line that answers why")
		}
	})
}

// No composite score, severity or environment label, held on the wire contract: a
// field absent from a template gets added back, one absent from the wire cannot be drawn.
func TestTheRowCarriesNoJudgement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.asset(t, h.org, "judged.target.test:443/tcp", map[string]any{
		"port": 443, "status_code": 200, "title": "App",
	})

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{Filter: filter(t, `{"op":"and"}`)})
		if err != nil {
			t.Fatalf("grouped list: %v", err)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		for _, forbidden := range []string{
			"score", "severity", "criticality", "risk", "environment", "priority", "grade",
		} {
			if strings.Contains(string(encoded), `"`+forbidden+`"`) {
				t.Errorf("the list carries a %q field, and none of those can be determined "+
					"from the outside", forbidden)
			}
		}
	})
}

// A script hash carries no badge and stays searchable and counted. It was a
// granularity error: a real inventory produced 464 hash badges across 50 rows.
func TestAScriptHashIsCountedWithoutABadge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	attributes := []byte(`{"script_hashes":["bundle"]}`)
	h.asset(t, h.org, "one.target.test", map[string]any{"attributes": attributes})
	h.asset(t, h.org, "two.target.test", map[string]any{"attributes": attributes})

	h.scoped(t, h.org, func(tx pgx.Tx) {
		page, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{Filter: filter(t, `{"op":"and"}`)})
		if err != nil {
			t.Fatalf("grouped list: %v", err)
		}
		found := false
		for _, group := range page.Groups {
			for _, row := range group.Rows {
				for _, pivot := range row.Pivots {
					if pivot.Type != "script" {
						continue
					}
					found = true
					if pivot.Count != 2 {
						t.Errorf("script counter = %d, want the two assets sharing it", pivot.Count)
					}
				}
			}
		}
		if !found {
			t.Error("the script pivot is absent from the row, so it is not searchable from it either")
		}

		// And it still filters, which is the half a removed badge must not cost.
		hits, err := search.List(ctx, tx, h.org, search.Request{
			Filter:  filter(t, `{"field":"script_hash","op":"contains","value":"bundle"}`),
			Display: true,
		})
		if err != nil {
			t.Fatalf("filter on the hash: %v", err)
		}
		if len(hits.Rows) != 2 {
			t.Errorf("a filter on the hash matched %d assets, want 2", len(hits.Rows))
		}
	})
}

// observe writes one journal row the way ingestion would.
func (h *harness) observe(t *testing.T, asset uuid.UUID, layer string, at, until time.Time, version, data string) {
	t.Helper()

	h.exec(t, `INSERT INTO observation (org_id, asset_id, observed_at, last_confirmed_at,
	                                    source, layer, outcome, producer_version, data)
	           VALUES ($1, $2, $3, $4, 'fastrecon', $5, 'ok', $6, $7::jsonb)`,
		h.org, asset, at, until, layer, version, data)
}

// arrival writes an asset with a chosen first_seen, which is what the feed
// orders on.
func (h *harness) arrival(t *testing.T, key string, at time.Time) uuid.UUID {
	t.Helper()

	id := h.asset(t, h.org, key, map[string]any{"last_seen": at})
	h.exec(t, `UPDATE asset_current SET first_seen = $2 WHERE asset_id = $1`, id, at)
	h.exec(t, `UPDATE asset SET first_seen = $2 WHERE id = $1`, id, at)
	return id
}

func port(key string) any {
	_, rest, found := strings.Cut(key, ":")
	if !found {
		return nil
	}
	number, _, _ := strings.Cut(rest, "/")
	value := 0
	for _, digit := range number {
		value = value*10 + int(digit-'0')
	}
	return value
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// The boundary between the list and the journal, shown from both sides. With SELECT
// ON observation revoked the list, facets and export must work, and the asset view
// must fail; otherwise the day the list starts reading the journal, the first half
// still passes because the privilege came back.
func TestTheListNeverTouchesTheJournal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := h.asset(t, h.org, "boundary.target.test:443/tcp", map[string]any{"port": 443})
	at := time.Now().UTC().Add(-time.Hour)
	h.observe(t, id, "http", at, at.Add(time.Minute), "1.0", `{"title":"App"}`)

	h.exec(t, `ALTER ROLE asm_app WITH LOGIN PASSWORD 'app-password-for-a-container'`)
	// Partitions included. Revoking on the parent leaves every partition
	// reachable by name, and this test would pass with the boundary open.
	h.exec(t, `REVOKE SELECT ON observation FROM asm_app`)
	h.exec(t, `DO $$
	           DECLARE part text;
	           BEGIN
	             FOR part IN SELECT c.relname FROM pg_class c
	                           JOIN pg_inherits i ON i.inhrelid = c.oid
	                           JOIN pg_class p ON p.oid = i.inhparent
	                          WHERE p.relname = 'observation'
	             LOOP
	               EXECUTE format('REVOKE SELECT ON %I FROM asm_app', part);
	             END LOOP;
	           END $$`)

	app := appPool(t, h.url)
	defer app.Close()

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin as the application role: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, h.org.String()); err != nil {
		t.Fatalf("scope: %v", err)
	}

	if _, err := search.ListGrouped(ctx, tx, h.org, search.GroupRequest{
		Filter: filter(t, `{"op":"and"}`),
	}); err != nil {
		t.Errorf("the list needs the journal: %v", err)
	}
	if _, err := search.Facets(ctx, tx, h.org, filter(t, `{"op":"and"}`)); err != nil {
		t.Errorf("the facets need the journal: %v", err)
	}
	if _, err := search.Get(ctx, tx, h.org, id, time.Now()); err == nil {
		t.Error("the asset view worked without SELECT on observation, so it is not " +
			"reading the journal and the timeline is coming from somewhere else")
	}
}

func appPool(t *testing.T, ownerURL string) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(ownerURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	url := fmt.Sprintf("postgres://asm_app:app-password-for-a-container@%s:%d/%s?sslmode=disable",
		cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool as asm_app: %v", err)
	}
	return pool
}
