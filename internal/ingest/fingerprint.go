package ingest

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/signals"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Rendered is what one render decided about an asset.
type Rendered struct {
	Lifecycle string
	Outcome   string
	Usable    bool
	// Page says a browser obtained something. It is what moves
	// last_fingerprint_at and nothing else does.
	Page         bool
	Deduplicated bool
	Next         time.Time
}

// Render writes what the browser saw.
//
// The rule it exists to respect: an error return is for a target the service
// could not address, and a target that refuses, times out or answers a
// challenge all come back as observations. When no candidate produces a chain,
// a browser receiving an invalid response on a port that speaks something other
// than HTTP, the service addressed the target perfectly well and the target
// answered something that is not HTTP. That is an observation with no page, and
// returning a bare error instead would leave the asset's counters untouched: its
// backoff never widens, its streak never moves, and the unobservable verdict
// this whole layer provides for becomes unreachable.
func (i *Ingestor) Render(
	ctx context.Context, q *sqlcgen.Queries, asset RenderTarget, result *fingerprint.Result,
) (Rendered, error) {
	st, err := i.renderState(ctx, q, asset)
	if err != nil {
		return Rendered{}, err
	}

	payload, err := toMap(result)
	if err != nil {
		return Rendered{}, err
	}

	final, page := result.Final()
	outcome := lifecycle.OutcomeOK
	usable := true

	if !page {
		// The service addressed the target and the target answered something
		// that is not a page. Nothing conclusive, and it is still a
		// measurement of the observer.
		outcome = lifecycle.OutcomeError
		usable = false
	} else {
		verdict := signals.Read(signals.Response{
			StatusCode: final.StatusCode,
			Server:     final.Headers["Server"],
			Title:      final.Title,
			Fronted:    asset.Fronted,
			Provider:   asset.Provider,
		})
		// A challenge page is the same page whichever client fetched it, and it
		// is what proves a challenge here. A technology in the WAF category
		// means "there is a WAF here", which Cloudflare reports on every
		// response it fronts including a normal 200: reading that as proof
		// would mark every legitimate 403 of a fronted application unmeasurable.
		if verdict.Dead != "" {
			outcome = lifecycle.OutcomeFail
		}
		usable = verdict.Usable()
	}

	at := i.now()
	obs := observation{
		layer:        normalize.LayerFingerprint,
		outcome:      outcome,
		data:         payload,
		usable:       &usable,
		takeoverKind: signals.KindUnclaimedService,
	}
	if page {
		obs.rendered = &at

		// The scheme the browser actually spoke, and it is a measurement rather
		// than a reading of the port: the request completed, so the service
		// answers there. It is what an imported service has no other way of
		// getting. A port scan reports an open port and no scheme, so the
		// console had nothing to name the asset with but `host:port` and
		// nothing to open, on assets a browser had already rendered.
		//
		// The target's scheme and never the final hop's: a redirect to another
		// host says what that host speaks, not this one.
		if parsed, err := url.Parse(asset.URL); err == nil {
			obs.scheme = parsed.Scheme
		}
	}

	// The render owns the response columns while the probe has never spoken.
	//
	// One producer per value still holds, and this is which one: the promoted
	// columns belong to whichever observer has measured a response, and an
	// asset whose http layer has never run has exactly one. Without it a
	// console showed `no answer · the probe obtained nothing` beside the 302 a
	// browser had brought back an hour earlier, because the chain travels as a
	// pivot and the status code did not. The probe takes the columns back on
	// its first pass, by the same rule and with no special case.
	//
	// Set whether or not a page came back, like the chain beside it: a render
	// that got nothing clears what a previous render wrote, where a COALESCE
	// would keep a status code its page no longer answers.
	if _, probed := st.layers[normalize.LayerHTTP]; !probed {
		obs.promote = true
		if page {
			obs.promoted = promoted{
				StatusCode: portPtr(final.StatusCode),
				Title:      text(final.Title),
				Server:     text(final.Headers["Server"]),
			}
			// Where it landed, and only when that is somewhere else. A final
			// url equal to the target is the ordinary case and repeating it
			// would make every console read "lands on" its own address.
			if final.URL != asset.URL {
				obs.promoted.FinalURL = text(final.URL)
			}
		}
	}

	summary := Summary{Unknown: map[string]int{}, grace: asset.Grace}
	if err := i.applyRender(ctx, q, asset, result, st, obs, &summary); err != nil {
		return Rendered{}, err
	}

	// A render produces events like any other observation, and they are written
	// in the same transaction. Leaving this out made every detection the
	// browser improved on invisible, which is the one classification the
	// instrument's version exists for.
	if err := i.writeEvents(ctx, q, Run{OrgID: asset.OrgID, ProgramID: asset.ProgramID}, &summary); err != nil {
		return Rendered{}, err
	}

	// The regime decides the cadence, and it is read after this observation
	// rather than before: a render that just flipped an asset onto the sole
	// detector regime has to be scheduled as one.
	next := at.Add(i.cadence.Spread(i.cadence.Render(st.regime), i.random()))
	// Back to the low queue whatever brought it here. A render that was urgent
	// has happened, and leaving the priority raised would keep an asset ahead
	// of the queue for every pass afterwards.
	if err := q.RescheduleRender(ctx, sqlcgen.RescheduleRenderParams{
		AssetID:  uuidTo(asset.AssetID),
		At:       stamp(next),
		Priority: lifecycle.PriorityBaseline,
	}); err != nil {
		return Rendered{}, fmt.Errorf("reschedule render of %s: %w", asset.Key, err)
	}

	return Rendered{
		Lifecycle:    st.lifecycle,
		Outcome:      outcome,
		Usable:       usable,
		Page:         page,
		Deduplicated: summary.Deduplicated > 0,
		Next:         next,
	}, nil
}

// RenderTarget is the asset a render was made for.
type RenderTarget struct {
	AssetID   uuid.UUID
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	Kind      string
	Key       string
	URL       string
	Fronted   bool
	Provider  string
	// Grace is what this programme's first run may keep quiet, frozen by the
	// caller for the same reason ingestion freezes it: a decision taken at
	// drain time would send a first run's flood late rather than never.
	Grace notify.Grace
}

// renderState reads the asset's counters without writing an identity.
//
// A render never creates an asset. The upsert path exists because a report
// discovers things; a render is made for an asset that already exists, and
// going through the same statement would let the renderer invent inventory.
func (i *Ingestor) renderState(ctx context.Context, q *sqlcgen.Queries, asset RenderTarget) (*state, error) {
	row, err := q.AssetForRender(ctx, sqlcgen.AssetForRenderParams{AssetID: uuidTo(asset.AssetID)})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", asset.Key, err)
	}
	var port int
	if row.Port != nil {
		port = int(*row.Port)
	}
	st, err := newState(sqlcgen.UpsertAssetAndProjectionRow{
		AssetID:                      row.AssetID,
		PreviousLifecycle:            &row.Lifecycle,
		PreviousBackoffTier:          &row.BackoffTier,
		PreviousHttpStreak:           &row.HttpStreak,
		PreviousFingerprintStreak:    &row.FingerprintStreak,
		PreviousHttpReachable:        row.HttpReachable,
		PreviousFingerprintReachable: row.FingerprintReachable,
		PreviousFirstSeen:            row.FirstSeen,
		PreviousLayers:               row.Layers,
	}, normalize.Kind(asset.Kind), port)
	if err != nil {
		return nil, err
	}
	// A render is made for an asset that already exists, so its identity comes
	// from the target rather than from a write. Without it every event a render
	// produces names nothing.
	st.key = asset.Key
	return st, nil
}

// applyRender writes the observation, its counters and the schedule.
func (i *Ingestor) applyRender(
	ctx context.Context, q *sqlcgen.Queries, asset RenderTarget,
	result *fingerprint.Result, st *state, obs observation, summary *Summary,
) error {
	run := Run{ID: uuid.Nil, OrgID: asset.OrgID, ProgramID: asset.ProgramID}
	report := Report{Run: RunInfo{Version: result.Version}}
	return i.apply(ctx, q, run, report, st, obs, summary)
}
