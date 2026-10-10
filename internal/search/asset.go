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
// The interface queries asset_current; observations serve history and diff. A
// timeline is history, which the projection cannot carry as it keeps only the
// current state. The list and facets, which run over a million rows and on every
// keystroke, never touch the journal; this opens on one asset, on demand, over
// an existing index.

// TimelineWindow is how far back the view reads. A constant: a caller able to
// request an hourly-probed asset's whole year is able to request the query this
// bound prevents.
const TimelineWindow = 90 * 24 * time.Hour

// TimelineCap is how many entries one layer contributes. A cut is reported but
// the window never is: the cap hides entries inside the window, a fact about
// this render, while the window is a property of the view.
const TimelineCap = 50

// ErrNoAsset is an identifier naming nothing this tenant can see. "No such asset"
// and "another organization's asset" are one error, since telling them apart
// enumerates what exists.
var ErrNoAsset = errors.New("no such asset")

// Evidence is the last observation of one layer, whole.
type Evidence struct {
	Layer   string `json:"layer"`
	Outcome string `json:"outcome"`
	Source  string `json:"source"`
	// ObservedAt is when this state began; LastConfirmedAt is the last probe that
	// found it unchanged. Unlabeled side by side they read as stale data.
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
	// Diff is absent on the oldest entry read for a layer. That is not "nothing
	// changed": it is the first row in the window, so what it moved from is outside
	// it, and the screen says so.
	Diff *Diff `json:"diff,omitempty"`
}

// Diff is what the comparison said about one transition.
type Diff struct {
	// Class carries the Notifier's reading, not a second one. A revelation is not an
	// alert: the instrument sees better and the target did not move.
	Class                   string        `json:"class"`
	PreviousProducerVersion *string       `json:"previous_producer_version,omitempty"`
	Fields                  []diff.Change `json:"fields"`
}

// Classes a transition can be given.
const (
	// ClassReal is the world moving.
	ClassReal = "real_change"
	// ClassDetection is the observer moving, claimable only by the fingerprint
	// layer. The scanner version is stamped on the other three too, so a new
	// open port would read as a detection improvement for a pass after every
	// scanner upgrade.
	ClassDetection = "detection_improved"
)

// Detail is what the asset view renders.
type Detail struct {
	Asset    Row        `json:"asset"`
	Evidence []Evidence `json:"evidence"`
	Timeline []Change   `json:"timeline"`
	// Truncated names the layers the cap cut; "truncated" on a four-panel page does
	// not say which to distrust.
	Truncated  []string          `json:"truncated_layers,omitempty"`
	WindowFrom time.Time         `json:"window_from"`
	Favicons   map[string]string `json:"favicons,omitempty"`
}

// One reads a single asset's projection with the list's columns, so this page's
// header and the row it was opened from cannot disagree.
func One(ctx context.Context, q Querier, org, asset uuid.UUID) (Row, error) {
	// Display on: the same badges as the row. A value the denylist removed from a
	// row must not reappear here as if it had always been worth one.
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

// Get reads one asset with its evidence and timeline. now is a parameter: the
// window is computed by the application and compared with timestamps the
// database wrote, and mixing two clocks depends on them agreeing.
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

	// Evidence is the newest entry of each layer, the timeline every entry with what
	// moved on the way in. One read serves both; a second query for each layer's
	// head would be four more round trips.
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
		// The next row in this ordering is the layer's previous state. The journal is
		// deduplicated on write, so two consecutive rows of one (asset, layer) are
		// distinct states and each is a change.
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
	// beyond marks the one row past the cap. It is read as the comparison partner of
	// the last displayed entry and never displayed; otherwise that entry would say
	// "not compared" with the previous state one row away.
	beyond bool
}

// older finds the previous state of the same layer: the next row of that layer
// in a newest-first ordering.
func older(entries []entry, from int) (entry, bool) {
	for i := from + 1; i < len(entries); i++ {
		if entries[i].layer == entries[from].layer {
			return entries[i], true
		}
	}
	return entry{}, false
}

// compare calls the same function the Notifier calls, on the same pair. A
// second comparator would show a different change from yesterday's alert, a
// divergence nobody notices until they must explain which is right.
func compare(previous, current entry) *Diff {
	changes := diff.Compare(previous.data, current.data)
	if len(changes) == 0 {
		// Two consecutive rows that compare equal. Deduplication compares the whole
		// payload while the diff ignores scan counters, so a row that only moved those
		// is a new state with nothing to show. Rare, not impossible.
		return nil
	}

	out := &Diff{Class: ClassReal, Fields: changes, PreviousProducerVersion: previous.version}
	if current.layer == string(normalize.LayerFingerprint) &&
		diff.Revelation(changes, text(previous.version), text(current.version)) {
		out.Class = ClassDetection
	}
	return out
}

// journal reads the window, capped per layer, and says which layers it cut. One
// statement with a window function, not one per layer. The cap is read as cap+1
// so the cut is known from the rows, as the list knows it has a next page.
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
			// The extra row proves there is more. It stays in the slice as the last
			// displayed entry's comparison partner; the flag keeps it off the page.
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
