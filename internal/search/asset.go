package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/diff"
	"github.com/JoshuaMart/recon/internal/normalize"
)

// The asset view, and the one read path that touches the journal.
//
// Principle 1 does not say nobody reads observations. It says the interface
// queries asset_current and that observations serve history and diff. A
// timeline of changes is history, and it is the one thing the projection cannot
// carry since by construction it keeps only a current state.
//
// What stays true is the substance of the rule: the list and the facets never
// touch the journal. They are what runs over a million rows and on every
// keystroke. This opens on one asset, on demand, and reads an index that
// already exists.

// TimelineWindow is how far back the view reads.
//
// A constant rather than a parameter. A caller able to ask for the whole
// journal of an asset probed hourly for a year is a caller able to ask for the
// query this bound exists to prevent.
const TimelineWindow = 90 * 24 * time.Hour

// TimelineCap is how many entries one layer contributes.
//
// It is reported when it cuts, and the window never is. The two absences are
// not the same absence: the cap cutting means there is more inside the window
// this page is not showing, which is a fact about this render. The window is a
// property of the view rather than of the asset.
const TimelineCap = 50

// ErrNoAsset is an identifier that names nothing this tenant can see.
//
// One error for "no such asset" and for "another organization's asset", because
// the two must be indistinguishable: the difference between them enumerates
// what exists.
var ErrNoAsset = errors.New("no such asset")

// Evidence is the last observation of one layer, whole.
type Evidence struct {
	Layer   string `json:"layer"`
	Outcome string `json:"outcome"`
	Source  string `json:"source"`
	// ObservedAt and LastConfirmedAt are two sentences. One is when this state
	// began, the other is the last probe that found it unchanged. Side by side
	// and unnamed they read as stale data, so both are written.
	ObservedAt      time.Time `json:"observed_at"`
	LastConfirmedAt time.Time `json:"last_confirmed_at"`
	ProducerVersion *string   `json:"producer_version,omitempty"`
	Data            any       `json:"data"`
}

// Change is one entry of the timeline: a state, how long it held, and what
// moved on the way in.
type Change struct {
	Layer           string    `json:"layer"`
	At              time.Time `json:"at"`
	HeldUntil       time.Time `json:"held_until"`
	Outcome         string    `json:"outcome"`
	ProducerVersion *string   `json:"producer_version,omitempty"`
	// Diff is absent on the oldest entry read for a layer, and that is not
	// "nothing changed". It is the first row inside the window, so what it
	// moved from is outside it, and the screen has to say which.
	Diff *Diff `json:"diff,omitempty"`
}

// Diff is what the comparison said about one transition.
type Diff struct {
	// Class carries the Notifier's reading rather than a second one. A
	// revelation is not an alert: the instrument sees better and the target did
	// not move.
	Class                   string        `json:"class"`
	PreviousProducerVersion *string       `json:"previous_producer_version,omitempty"`
	Fields                  []diff.Change `json:"fields"`
}

// Classes a transition can be given.
const (
	// ClassReal is the world moving.
	ClassReal = "real_change"
	// ClassDetection is the observer moving, which the fingerprint layer is the
	// only one that can claim. The scanner's version is stamped on the other
	// three too, so applying it there would read a newly opened port as a
	// detection improvement for one pass after every scanner upgrade.
	ClassDetection = "detection_improved"
)

// Detail is what the asset view renders.
type Detail struct {
	Asset    Row        `json:"asset"`
	Evidence []Evidence `json:"evidence"`
	Timeline []Change   `json:"timeline"`
	// Truncated names the layers the cap cut, because "the timeline was
	// truncated" on a page with four panels does not say which one to distrust.
	Truncated  []string          `json:"truncated_layers,omitempty"`
	WindowFrom time.Time         `json:"window_from"`
	Favicons   map[string]string `json:"favicons,omitempty"`
}

// One reads a single asset's projection.
//
// The same columns as the list, so the header of this page and the row it was
// opened from cannot disagree.
func One(ctx context.Context, q Querier, org, asset uuid.UUID) (Row, error) {
	// Display on. This page shows the same badges the row showed, and a value
	// the denylist removed from a row must not reappear here as if it had been
	// worth one all along.
	sql := "SELECT" + selectColumns + " FROM asset_current " + Alias + lineageJoin +
		fmt.Sprintf(pivotJoin, 3) +
		" WHERE " + Alias + ".org_id = $1 AND " + Alias + ".asset_id = $2"

	rows, err := q.Query(ctx, sql, org, asset, true)
	if err != nil {
		return Row{}, fmt.Errorf("run the asset read: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Row{}, fmt.Errorf("read the asset: %w", err)
		}
		return Row{}, ErrNoAsset
	}
	row, err := scanRow(rows)
	if err != nil {
		return Row{}, err
	}
	return row, nil
}

// Get reads one asset with its evidence and its timeline.
//
// now is a parameter rather than time.Now(), for the reason every date
// comparison in this repository takes one: the window is computed by the
// application and compared against timestamps the database wrote, and a
// comparison that mixes two clocks depends on them agreeing.
func Get(ctx context.Context, q Querier, org, asset uuid.UUID, now time.Time) (Detail, error) {
	row, err := One(ctx, q, org, asset)
	if err != nil {
		return Detail{}, err
	}

	from := now.Add(-TimelineWindow)
	detail := Detail{
		Asset:      row,
		Evidence:   []Evidence{},
		Timeline:   []Change{},
		WindowFrom: from,
	}

	entries, cut, err := journal(ctx, q, org, asset, from)
	if err != nil {
		return Detail{}, err
	}
	detail.Truncated = cut

	// The evidence is the newest entry of each layer, and the timeline is every
	// entry with what moved on the way in. Both come out of the same read: a
	// second query for the head of each layer would be four more round trips
	// for rows already in hand.
	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.beyond {
			continue
		}
		if !seen[entry.layer] {
			seen[entry.layer] = true
			detail.Evidence = append(detail.Evidence, Evidence{
				Layer:           entry.layer,
				Outcome:         entry.outcome,
				Source:          entry.source,
				ObservedAt:      entry.observedAt,
				LastConfirmedAt: entry.lastConfirmedAt,
				ProducerVersion: entry.version,
				Data:            entry.raw,
			})
		}

		change := Change{
			Layer:           entry.layer,
			At:              entry.observedAt,
			HeldUntil:       entry.lastConfirmedAt,
			Outcome:         entry.outcome,
			ProducerVersion: entry.version,
		}
		// The row that follows in this ordering is the previous state of the
		// same layer, since the journal is deduplicated on write: two
		// consecutive rows of one (asset, layer) are two distinct states by
		// construction, so each row is a change.
		if previous, found := older(entries, i); found {
			change.Diff = compare(previous, entry)
		}
		detail.Timeline = append(detail.Timeline, change)
	}

	images, err := Favicons(ctx, q, org, []Row{row})
	if err != nil {
		return Detail{}, err
	}
	detail.Favicons = images
	return detail, nil
}

// entry is one journal row, decoded.
type entry struct {
	layer           string
	outcome         string
	source          string
	observedAt      time.Time
	lastConfirmedAt time.Time
	version         *string
	previousVersion *string
	raw             any
	data            map[string]any
	// beyond marks the one row past the cap. It is read so the last displayed
	// entry of a cut layer has something to compare against, and never
	// displayed: without it that entry would say "not compared" where the
	// previous state is right there, one row away.
	beyond bool
}

// older finds the previous state of the same layer, which is the next row of
// that layer in an ordering already newest first.
func older(entries []entry, from int) (entry, bool) {
	for i := from + 1; i < len(entries); i++ {
		if entries[i].layer == entries[from].layer {
			return entries[i], true
		}
	}
	return entry{}, false
}

// compare calls the same function the Notifier calls, on the same pair.
//
// Writing a second comparator for the screen would give an interface showing a
// different change from the alert received yesterday, and that is the kind of
// divergence nobody notices until they have to explain which of the two is
// right.
func compare(previous, current entry) *Diff {
	changes := diff.Compare(previous.data, current.data)
	if len(changes) == 0 {
		// Two consecutive rows that compare equal. Deduplication makes this
		// rare rather than impossible, since it compares the whole payload and
		// the diff ignores the scan counters, so a row that only moved those is
		// a new state with nothing to show for it.
		return nil
	}

	out := &Diff{Class: ClassReal, Fields: changes, PreviousProducerVersion: previous.version}
	if current.layer == string(normalize.LayerFingerprint) &&
		diff.Revelation(changes, text(previous.version), text(current.version)) {
		out.Class = ClassDetection
	}
	return out
}

// journal reads the window, capped per layer, and says which layers it cut.
//
// One statement with a window function rather than one per layer. The cap is
// read as cap+1 so the cut is known from the rows themselves, which is the same
// trick the list uses to know it has a next page without counting.
func journal(
	ctx context.Context, q Querier, org, asset uuid.UUID, from time.Time,
) ([]entry, []string, error) {
	rows, err := q.Query(ctx, `
		SELECT layer, outcome, source, observed_at, last_confirmed_at,
		       producer_version, last_producer_version, data, rank
		  FROM (
		        SELECT o.layer, o.outcome, o.source, o.observed_at, o.last_confirmed_at,
		               o.producer_version, o.last_producer_version, o.data,
		               row_number() OVER (PARTITION BY o.layer
		                                      ORDER BY o.observed_at DESC) AS rank
		          FROM observation o
		         WHERE o.org_id = $1 AND o.asset_id = $2 AND o.observed_at >= $3
		       ) windowed
		 WHERE rank <= $4
		 ORDER BY observed_at DESC, layer`,
		org, asset, from, TimelineCap+1)
	if err != nil {
		return nil, nil, fmt.Errorf("run the journal read: %w", err)
	}
	defer rows.Close()

	entries := make([]entry, 0, TimelineCap)
	cutBy := map[string]bool{}
	for rows.Next() {
		var row entry
		var payload []byte
		var rank int
		if err := rows.Scan(&row.layer, &row.outcome, &row.source,
			&row.observedAt, &row.lastConfirmedAt,
			&row.version, &row.previousVersion, &payload, &rank); err != nil {
			return nil, nil, fmt.Errorf("read a journal row: %w", err)
		}
		if rank > TimelineCap {
			// The extra row is the proof there is more. It stays in the slice
			// as the comparison partner of the last displayed entry, and the
			// flag is what keeps it off the page.
			cutBy[row.layer] = true
			row.beyond = true
		}
		if err := json.Unmarshal(payload, &row.data); err != nil {
			return nil, nil, fmt.Errorf("decode a %s observation: %w", row.layer, err)
		}
		row.raw = json.RawMessage(payload)
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the journal: %w", err)
	}

	cut := make([]string, 0, len(cutBy))
	for _, layer := range []string{
		string(normalize.LayerDNS), string(normalize.LayerTCP),
		string(normalize.LayerHTTP), string(normalize.LayerFingerprint),
	} {
		if cutBy[layer] {
			cut = append(cut, layer)
		}
	}
	if len(cut) == 0 {
		cut = nil
	}
	return entries, cut, nil
}
