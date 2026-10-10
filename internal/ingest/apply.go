package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/signals"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// state is one asset's memory across the observations of one report, so
// transitions are decided over all layers at once. Reading one layer would give
// opposite answers depending on which observation was written last.
type state struct {
	id      uuid.UUID
	key     string
	kind    normalize.Kind
	created bool
	// lineage is returned by the upsert and carried by every notification.
	lineage []byte
	// source is the asset's own discovery source, from its first appearance. The
	// grace reads it, not the observation's: a typed-in asset is born from a probe
	// observation, which would make the hand-fed branch dead code in production.
	source string
	// announced is the state already told for. An asset has one transition per
	// report but several observations, so emitting per layer would notify the same
	// arrival repeatedly. It holds the state, not a flag, so an asset moving again
	// within one report (active, then inactive) still says where it ended up.
	announced string
	// previousLifecycle is the state before this report touched the asset; reading
	// the column afterwards would compare a state with itself.
	previousLifecycle string
	lifecycle         string
	tier              int
	// reach is what each observer has managed; regime is what the projection has
	// settled on. reach moves on every observation, regime after three concordant ones.
	reach  lifecycle.Reach
	regime lifecycle.Regime
	// renderable says a browser will open this asset's port at all. It travels with
	// the state because the promote path decides in the same transaction.
	renderable bool
	firstSeen  time.Time
	layers     map[normalize.Layer]lifecycle.Counters
}

// storedLayer is how a layer's counters travel back from the upsert.
type storedLayer struct {
	State          string     `json:"state"`
	Informative    int        `json:"informative"`
	NonInformative int        `json:"non_informative"`
	FirstFailureAt *time.Time `json:"first_failure_at"`
	LastOKAt       *time.Time `json:"last_ok_at"`
	LastCheckedAt  *time.Time `json:"last_checked_at"`
}

func newState(row sqlcgen.UpsertAssetAndProjectionRow, kind normalize.Kind, port int) (*state, error) {
	st := &state{
		id:         uuid.UUID(row.AssetID.Bytes),
		kind:       kind,
		created:    row.Created,
		renderable: port > 0 && fingerprint.Renderable(port),
		layers:     map[normalize.Layer]lifecycle.Counters{},
	}
	if row.PreviousDiscoverySource != nil {
		st.source = *row.PreviousDiscoverySource
	}
	if row.PreviousLifecycle != nil {
		st.lifecycle = *row.PreviousLifecycle
		st.previousLifecycle = *row.PreviousLifecycle
	}
	if row.PreviousBackoffTier != nil {
		st.tier = int(*row.PreviousBackoffTier)
	}
	if row.PreviousHttpStreak != nil {
		st.reach.HTTP = int(*row.PreviousHttpStreak)
	}
	if row.PreviousFingerprintStreak != nil {
		st.reach.Fingerprint = int(*row.PreviousFingerprintStreak)
	}
	st.regime = lifecycle.Regime{
		HTTP:        row.PreviousHttpReachable,
		Fingerprint: row.PreviousFingerprintReachable,
	}
	if row.PreviousFirstSeen.Valid {
		st.firstSeen = row.PreviousFirstSeen.Time
	}

	if len(row.PreviousLayers) == 0 {
		return st, nil
	}
	var stored map[string]storedLayer
	if err := json.Unmarshal(row.PreviousLayers, &stored); err != nil {
		return nil, fmt.Errorf("decode layer counters: %w", err)
	}
	for name, layer := range stored {
		st.layers[normalize.Layer(name)] = lifecycle.Counters{
			State:          layer.State,
			Informative:    layer.Informative,
			NonInformative: layer.NonInformative,
			FirstFailureAt: deref(layer.FirstFailureAt),
			LastOKAt:       deref(layer.LastOKAt),
			LastCheckedAt:  deref(layer.LastCheckedAt),
		}
	}
	return st, nil
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// decide folds one qualified outcome in and returns what the asset now is.
func (s *state) decide(layer normalize.Layer, outcome string, at time.Time) (lifecycle.Counters, string) {
	counters := lifecycle.Next(s.layers[layer], outcome, at)
	s.layers[layer] = counters

	all := make([]lifecycle.Counters, 0, len(s.layers))
	for _, c := range s.layers {
		all = append(all, c)
	}

	current := s.lifecycle
	// A rediscovery is a success on an asset nothing was watching. It is cleared
	// here, not in Decide, which sees only counters: an archived asset still
	// carrying a healthy layer would be revived by a timeout.
	if lifecycle.Revived(current, outcome) {
		current = ""
	}
	// Scheduled separates hosts, which the budget can archive, from services and
	// URLs, which the rescheduling path never touches.
	scheduled := s.kind == normalize.KindFQDN || s.kind == normalize.KindIP
	s.lifecycle = lifecycle.Decide(current, scheduled, s.reach, all...)
	return counters, s.lifecycle
}

// observe folds one usable reading into the observer's counter.
//
// The streak updates before the state is decided, so leaving unobservable reads
// the current observation: one probe getting through crosses zero in the same pass.
func (s *state) observe(layer normalize.Layer, usable bool) (int32, *bool) {
	var streak *int
	var settled **bool
	switch layer {
	case normalize.LayerHTTP:
		streak, settled = &s.reach.HTTP, &s.regime.HTTP
	case normalize.LayerFingerprint:
		streak, settled = &s.reach.Fingerprint, &s.regime.Fingerprint
	default:
		return 0, nil
	}

	*streak = nextStreak(*streak, usable)
	if *streak >= lifecycle.ReachThreshold || *streak <= -lifecycle.ReachThreshold {
		flipped := *streak > 0
		*settled = &flipped
		return counter(*streak), &flipped
	}
	return counter(*streak), nil
}

// observation is one thing to write about one asset.
type observation struct {
	layer   normalize.Layer
	outcome string
	data    map[string]any

	// promote says this layer owns the promoted columns. Explicit rather than
	// COALESCE: a coalesced title would survive its own page forever.
	promote  bool
	promoted promoted

	// scheme is the one this observation spoke, set by the observer that completed
	// a request, not derived from a port. Empty on layers that establish none, so a
	// null never erases a measured scheme.
	scheme string

	// usable answers "did my probe get anything out of this?", orthogonal to the
	// outcome. Nil on a layer that says nothing about an observer's reach.
	usable *bool

	takeover *signals.Takeover
	// takeoverKind is what this layer could have found, so a finding is cleared
	// only by the layer that produced it.
	takeoverKind string

	// rendered is when a browser actually obtained a page. Separate from the
	// observation instant: a render that got nothing is still an observation, and
	// moving the timestamp would claim a page nobody saw.
	rendered *time.Time
}

// promoted is what the search API filters on.
type promoted struct {
	StatusCode   *int32
	FinalURL     *string
	Title        *string
	Server       *string
	Technologies []string
	WAFDetected  *bool
	WAFVendor    *string
	// Structural rather than per response: filled by whichever layer can see it and
	// re-evaluated every pass.
	IsCDN       *bool
	CDNProvider *string
}

// apply writes one observation and everything it decides.
func (i *Ingestor) apply(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	st *state, obs observation, summary *Summary,
) error {
	result, err := normalize.Payload(obs.layer, obs.data)
	if err != nil {
		return fmt.Errorf("normalize %s: %w", obs.layer, err)
	}
	for _, name := range result.Unknown {
		summary.Unknown[string(obs.layer)+"."+name]++
	}

	encoded, err := json.Marshal(result.Data)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", obs.layer, err)
	}

	at := i.now()
	outcome := qualify(obs.outcome, report)

	// The observer's counter moves before the state is decided. Entering and
	// leaving unobservable both read the current observation; deciding first would
	// enter and leave one observation late, two rounds on a threshold of three.
	var streak *int32
	var reachable *bool
	if obs.usable != nil {
		value, flag := st.observe(obs.layer, *obs.usable)
		streak, reachable = &value, flag
	}

	counters, decided := st.decide(obs.layer, outcome, at)

	params := sqlcgen.WriteObservationParams{
		OrgID:          uuidTo(run.OrgID),
		AssetID:        uuidTo(st.id),
		ObservedAt:     stamp(at),
		Source:         "fastrecon",
		Layer:          string(obs.layer),
		Outcome:        outcome,
		Data:           encoded,
		LayerState:     counters.State,
		Informative:    counter(counters.Informative),
		NonInformative: counter(counters.NonInformative),
		Lifecycle:      decided,
		Promote:        obs.promote,
		TakeoverKind:   obs.takeoverKind,
	}
	// Null, not a zero uuid: a render belongs to no run, and a populated column
	// naming nothing is worse than an empty one.
	if run.ID != uuid.Nil {
		params.RunID = uuidTo(run.ID)
	}
	if !counters.FirstFailureAt.IsZero() {
		params.FirstFailureAt = stamp(counters.FirstFailureAt)
	}
	if !counters.LastOKAt.IsZero() {
		params.LastOkAt = stamp(counters.LastOKAt)
	}
	if report.Run.Version != "" {
		params.ProducerVersion = text(report.Run.Version)
	}
	if obs.promote {
		params.StatusCode = obs.promoted.StatusCode
		params.FinalUrl = obs.promoted.FinalURL
		params.Title = obs.promoted.Title
		params.Server = obs.promoted.Server
		params.Technologies = obs.promoted.Technologies
		params.WafDetected = obs.promoted.WAFDetected
		params.WafVendor = obs.promoted.WAFVendor
	}
	params.IsCdn = obs.promoted.IsCDN
	params.CdnProvider = obs.promoted.CDNProvider
	// Outside the promote flag: the scheme is kept once established, offered by
	// whichever layer measured one and never cleared by one that did not.
	params.Scheme = text(obs.scheme)

	// The signed counter and the flag it flips. Three concordant results are needed
	// in both directions, so one bad pass never moves the regime.
	if streak != nil {
		switch obs.layer {
		case normalize.LayerHTTP:
			params.HttpStreak, params.HttpReachable = streak, reachable
		case normalize.LayerFingerprint:
			params.FingerprintStreak, params.FingerprintReachable = streak, reachable
		}
	}
	// Follows the render, not the observation: moves when the payload carries a
	// final hop, i.e. when a browser obtained a page.
	if obs.rendered != nil {
		params.LastFingerprintAt = stamp(*obs.rendered)
	}
	if obs.takeover != nil {
		finding, err := json.Marshal(obs.takeover)
		if err != nil {
			return fmt.Errorf("encode takeover candidate: %w", err)
		}
		params.Takeover = finding
	}

	// Pivots come from the normalized payload, which is what the journal stores;
	// anything else could make projection and journal disagree.
	lifted := liftPivots(obs.layer, result.Data)
	params.FaviconHash = lifted.FaviconHash
	params.ScriptHashes = lifted.ScriptHashes
	params.CookieNames = lifted.CookieNames
	params.ExternalHosts = lifted.ExternalHosts
	params.TechRender = lifted.TechRender
	params.CertSpkiHash = lifted.CertSPKIHash
	params.StatusChain = lifted.StatusChain

	row, err := q.WriteObservation(ctx, params)
	if err != nil {
		return fmt.Errorf("write %s observation: %w", obs.layer, err)
	}
	if !row.Projected {
		// A missing row means identity and projection have diverged, worse than a
		// failed write because nothing downstream would notice.
		return fmt.Errorf("observation on %s wrote no projection", st.id)
	}

	summary.Observations++

	// The image goes in the same transaction as the projection naming its hash. It
	// is its own statement because it carries bytes, only on the render path and
	// only when the service inlined one.
	if obs.layer == normalize.LayerFingerprint {
		if image, ok := liftFavicon(result.Data); ok {
			if err := q.StoreFavicon(ctx, sqlcgen.StoreFaviconParams{
				OrgID:     uuidTo(run.OrgID),
				Hash:      image.Hash,
				MediaType: image.MediaType,
				Bytes:     image.Bytes,
			}); err != nil {
				return fmt.Errorf("store the favicon of %s, %s: %w", st.id, image.describe(), err)
			}
		}
	}

	// What the asset became is told whether or not a row was written. A death is
	// three identical nxdomains and lands on the third; telling only where a row
	// was written would silence the most common death.
	became := st.announced
	i.announce(run, st, obs, st.source, summary)

	// A name going quiet while production pages still load scripts from it is the
	// supply chain case. No payload comparison says it, the transition does, so it
	// is read here and not in the diff.
	if became != st.announced && st.lifecycle == lifecycle.Inactive {
		if err := i.externalReferrers(ctx, q, run, st, st.source, summary); err != nil {
			return err
		}
	}

	if row.Deduplicated {
		summary.Deduplicated++
		return nil
	}

	// Only an insertion has a diff. The previous payload comes back on that path
	// alone; most observations deduplicate and have nothing to compare.
	var before map[string]any
	if len(row.PreviousData) > 0 {
		if err := json.Unmarshal(row.PreviousData, &before); err != nil {
			return fmt.Errorf("decode the previous %s payload: %w", obs.layer, err)
		}
	}
	previousVersion := ""
	if row.PreviousProducerVersion != nil {
		previousVersion = *row.PreviousProducerVersion
	}
	// Compared on the normalized structure, never the arrival form. The stored side
	// is normalized, so comparing the raw side would report everything normalization
	// touches (a dropped version, a cookie map turned into names) as a change.
	normalized := obs
	normalized.data = result.Data
	i.diffEvents(run, st, normalized, before, previousVersion, report.Run.Version, st.source, summary)

	// The other direction: this page points at a host already declared dead. Insert
	// path only, like the takeover finding, so a repeated render does not re-alert
	// for as long as the reference lasts.
	if err := i.externalReferences(ctx, q, run, st, normalized, st.source, summary); err != nil {
		return err
	}

	// A detected HTTP change buys a render, only in the nominal regime: when the raw client is
	// turned away, what it sees of a refusing target is not worth a browser. A first observation
	// has its own trigger, filter and queue; promoting it here would flood the queue meant to
	// stay short. Same filter as the baseline, or a change on a service Chrome refuses to open
	// would take the queue head and stay there.
	if obs.layer == normalize.LayerHTTP && st.regime.Detector() &&
		row.PreviousData != nil && st.renderable {
		if err := q.PromoteRender(ctx, sqlcgen.PromoteRenderParams{
			AssetID:  uuidTo(st.id),
			At:       stamp(at),
			Priority: lifecycle.PriorityChange,
		}); err != nil {
			return fmt.Errorf("promote render of %s: %w", st.id, err)
		}
	}
	return nil
}

// counter narrows a running total to its column. The clamp is not decoration:
// an overflow would arrive as a negative streak reading as the opposite.
func counter(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}

// nextStreak walks the signed counter. It crosses zero instead of
// decrementing, so one success after four failures reads as one success.
func nextStreak(current int, usable bool) int {
	if usable {
		return max(current, 0) + 1
	}
	return min(current, 0) - 1
}

// reschedule moves a host's due dates after a report answered for it. Only
// hosts: a service is observed through its host's run, so its own resolve date
// would sit in a queue nothing dispatches from.
func (i *Ingestor) reschedule(ctx context.Context, q *sqlcgen.Queries, run Run, st *state) error {
	if st.kind != normalize.KindFQDN && st.kind != normalize.KindIP {
		return nil
	}

	at := i.now()
	// An asset never alive ends archived, not inactive: it never existed.
	archive := st.lifecycle == lifecycle.Candidate && lifecycle.Exhausted(st.firstSeen, at)

	// A candidate that answers earns the full rung, and only this path can give it.
	// Without it a candidate has no full date, is checked only by resolve runs
	// (which leave that date alone) and is never port-swept. The failure would be
	// silent and total.
	promoted := st.previousLifecycle == lifecycle.Candidate && st.lifecycle != lifecycle.Candidate

	params := sqlcgen.RescheduleAssetParams{
		AssetID: uuidTo(st.id),
		Archive: archive,
		// The stored tier is the one the next failure reads; the delay below is the
		// rung this failure earned. Incrementing first would skip the first rung, one
		// minute on the candidate curve and the whole freshness advantage.
		BackoffTier: counter(lifecycle.NextTier(st.lifecycle, st.tier)),
		MoveResolve: true,
		MoveFull:    run.movesFull() || promoted,
	}
	if !archive {
		resolve := at.Add(i.cadence.Spread(i.cadence.Delay(st.lifecycle, lifecycle.RungResolve, st.tier), i.random()))
		params.NextResolveAt = stamp(resolve)
		if params.MoveFull {
			// A promotion is due now: the point is to see what a candidate exposes as it
			// appears. It takes the stagger, not the curve's spread, because it joins a
			// recurring cadence where a convoy is permanent.
			delay := i.cadence.Spread(i.cadence.Delay(st.lifecycle, lifecycle.RungFull, st.tier), i.random())
			if promoted && !run.movesFull() {
				delay = i.cadence.Stagger(i.random())
			}
			params.NextFullAt = stamp(at.Add(delay))
		}
	}

	if err := q.RescheduleAsset(ctx, params); err != nil {
		return fmt.Errorf("reschedule %s: %w", st.id, err)
	}
	return nil
}

// movesFull reports whether the run's scope reached the full rung. Only a
// resolve run leaves the full date alone; an asset due for full needs no
// resolve run, but not the reverse.
//
// The list once named the scopes that do sweep and missed the one every
// discovery run carries. A discovered host then had no full date and its ports
// were never swept again, so a newly opened port stayed invisible.
func (r Run) movesFull() bool { return r.Scope != lifecycle.RungResolve }
