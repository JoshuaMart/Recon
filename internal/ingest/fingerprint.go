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
	// Page says a browser obtained something; it alone moves last_fingerprint_at.
	Page         bool
	Deduplicated bool
	Next         time.Time
}

// Render writes what the browser saw.
//
// An error return is for a target the service could not address. A target that
// refuses, times out or answers a challenge comes back as an observation, and so
// does a port speaking something other than HTTP: the service addressed it fine
// and got a non-HTTP answer. A bare error there would leave the asset's counters
// untouched, its backoff never widening and the unobservable verdict unreachable.
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
		// The service addressed the target and got something that is not a page: not
		// conclusive, but still a measurement of the observer.
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
		// A challenge page is the same whichever client fetched it, and proves a
		// challenge. A WAF-category technology only means "a WAF is here" (Cloudflare
		// reports it on every response, even a normal 200), so reading it as proof would
		// mark every legitimate 403 of a fronted application unmeasurable.
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

		// The scheme the browser spoke: a measurement, not a reading of the port, since
		// the request completed. An imported service has no other source: a port scan
		// gives an open port and no scheme, leaving the console `host:port` and nothing
		// to open on assets already rendered.
		//
		// The target's scheme, never the final hop's: a redirect to another host says what
		// that host speaks.
		if parsed, err := url.Parse(asset.URL); err == nil {
			obs.scheme = parsed.Scheme
		}
	}

	// The render owns the response columns while the probe has never spoken. They belong to
	// whichever observer has measured a response, and an asset whose http layer never ran has
	// exactly one; otherwise the console showed `no answer` beside the 302 a browser had brought
	// back (the chain travels as a pivot, the status code did not). The probe takes the columns
	// back on its first pass by the same rule. Set whether or not a page came back, like the
	// chain: a render that got nothing clears what an earlier one wrote, where COALESCE would
	// keep a stale status code.
	if _, probed := st.layers[normalize.LayerHTTP]; !probed {
		obs.promote = true
		if page {
			obs.promoted = promoted{
				StatusCode: portPtr(final.StatusCode),
				Title:      text(final.Title),
				Server:     text(final.Headers["Server"]),
			}
			// Where it landed, only when elsewhere: a final URL equal to the target is the
			// ordinary case and would make every console read "lands on" its own address.
			if final.URL != asset.URL {
				obs.promoted.FinalURL = text(final.URL)
			}
		}
	}

	summary := Summary{Unknown: map[string]int{}, grace: asset.Grace}
	if err := i.applyRender(ctx, q, asset, result, st, obs, &summary); err != nil {
		return Rendered{}, err
	}

	// A render produces events like any observation, in the same transaction. Leaving
	// this out hid every detection the browser improved on, the one classification
	// the instrument's version exists for.
	if err := i.writeEvents(ctx, q, Run{OrgID: asset.OrgID, ProgramID: asset.ProgramID}, &summary); err != nil {
		return Rendered{}, err
	}

	// The regime decides the cadence, read after this observation: a render that just
	// put an asset on the sole-detector regime must be scheduled as one.
	next := at.Add(i.cadence.Spread(i.cadence.Render(st.regime), i.random()))
	// Back to the low queue whatever brought it here: an urgent render has happened,
	// and a raised priority would keep the asset ahead of the queue afterwards.
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
	// Grace is what this programme's first run may keep quiet, frozen by the caller
	// as in ingestion: deciding at drain time would send a first run's flood late.
	Grace notify.Grace
}

// renderState reads the asset's counters without writing an identity. A render
// never creates an asset: reports discover things, and the same statement would
// let the renderer invent inventory.
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
	// The identity comes from the target, not a write; without it every event a
	// render produces names nothing.
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
