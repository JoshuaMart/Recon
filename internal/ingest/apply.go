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

// state is one asset's memory, carried across the observations of one report.
//
// It exists so that the transitions are decided over the whole set of layers
// rather than one at a time. An asset whose dns layer is dead and whose tcp
// layer answers is inactive, and reading that off a single layer would give the
// opposite answer depending on which observation happened to be written last.
type state struct {
	id      uuid.UUID
	key     string
	kind    normalize.Kind
	created bool
	// lineage is what the upsert returned in passing. Every notification
	// carries it rather than just the current state, and it is in the row that
	// was just written, so it costs nothing to hold.
	lineage []byte
	// source is the asset's own discovery source, which is the one from its
	// first appearance. The grace reads it rather than the observation's: a
	// typed in asset is born from a probe observation, so reading that would
	// make the hand fed branch dead code in production while its test passes.
	source string
	// announced is the state already told for. An asset has
	// one transition per report and several observations, so emitting from
	// each layer would notify the same arrival two or three times over. It
	// holds the state rather than a flag so that an asset which moves again
	// inside one report, active and then inactive as a later layer reports a
	// death, still says so: otherwise the arrival stands as the only record
	// and it is the opposite of where the asset ended up.
	announced string
	// previousLifecycle is what this asset was before this report touched it.
	// A transition is the difference between the two, and reading the column
	// afterwards would compare a state with itself.
	previousLifecycle string
	lifecycle         string
	tier              int
	// reach is what each observer has managed, and regime is what the
	// projection has settled on. They are two different questions: the first
	// moves on every observation, the second only after three concordant ones.
	reach  lifecycle.Reach
	regime lifecycle.Regime
	// renderable says a browser will open this asset's port at all. It travels
	// with the state because the promote path decides in the same transaction
	// as the write, and the port is already in hand there.
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
	// A rediscovery is a success on an asset nothing was still watching. It is
	// cleared here rather than inside Decide because Decide sees only the
	// counters, and a layer holds the state of its last conclusive measurement:
	// an archived asset still carrying a healthy layer would be revived by a
	// timeout.
	if lifecycle.Revived(current, outcome) {
		current = ""
	}
	// Scheduled is what separates a host, which the budget can archive, from a
	// service or a URL, which the rescheduling path never touches.
	scheduled := s.kind == normalize.KindFQDN || s.kind == normalize.KindIP
	s.lifecycle = lifecycle.Decide(current, scheduled, s.reach, all...)
	return counters, s.lifecycle
}

// observe folds one usable reading into the observer's counter.
//
// The streak is updated before the state is decided, which is what makes
// leaving unobservable read the current observation rather than the column: one
// probe getting through crosses the counter back over zero in the same pass.
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

	// promote says this layer owns the promoted columns. It is explicit rather
	// than a COALESCE, because those two differ exactly when a page loses a
	// field: a coalesced title survives its own page forever.
	promote  bool
	promoted promoted

	// usable answers "did my probe get anything out of this?", which is
	// orthogonal to what the outcome answers about the target. Nil on a layer
	// that says nothing about an observer's reach.
	usable *bool

	takeover *signals.Takeover
	// takeoverKind is what this layer could have found, and it is what lets a
	// finding be cleared by the layer that produced it and by no other.
	takeoverKind string

	// rendered is when a browser actually obtained a page. Distinct from the
	// observation's own instant on purpose: a render that got nothing is still
	// an observation, and moving the render timestamp on one would make a list
	// claim a page nobody ever saw.
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
	// Structural rather than per response, so it is filled by whichever layer
	// can see it and re-evaluated on every pass.
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

	// The observer's counter moves before the state is decided, and the order
	// is the rule rather than an implementation detail. Both entering and
	// leaving unobservable read the current observation: a pass that decided
	// first would enter the state one observation late and leave it one late
	// too, which is two rounds on a threshold of three.
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
	// Null rather than a zero uuid on a render, which belongs to no run: a
	// column that looks populated and names nothing is worse than an empty one.
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

	// The signed counter, and the flag it flips. Three concordant results are
	// needed in both directions, so a single bad pass never moves the regime.
	if streak != nil {
		switch obs.layer {
		case normalize.LayerHTTP:
			params.HttpStreak, params.HttpReachable = streak, reachable
		case normalize.LayerFingerprint:
			params.FingerprintStreak, params.FingerprintReachable = streak, reachable
		}
	}
	// The timestamp follows the render, not the observation. It moves when the
	// payload carries a final hop, which is when a browser obtained a page.
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

	// The pivots, read from the normalized payload rather than from the
	// producer's document. The journal stores the normalized form, so reading
	// anything else here would let the projection and the journal disagree
	// about what was seen.
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
		// The projection is where every decision lands. A row that is not
		// there means the identity and its projection have diverged, which is
		// worse than a failed write because nothing downstream would notice.
		return fmt.Errorf("observation on %s wrote no projection", st.id)
	}

	summary.Observations++

	// The image, in the same transaction as the projection that names its hash.
	// A statement of its own because it carries bytes, and only where there is
	// an image to carry, which is the render path and only when the service
	// inlined one.
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

	// What the asset became is told whether or not a row was written. A death
	// is three identical nxdomains and the transition lands on the third, so
	// producing it only where a row was written would make the most common
	// death in the system silent.
	became := st.announced
	i.announce(run, st, obs, st.source, summary)

	// A name going quiet while production pages still load scripts from it is
	// the supply chain case with the step where somebody has to visit already
	// taken. Nothing in a payload comparison says it: the transition is what
	// does, so it is read here and not in the diff.
	if became != st.announced && st.lifecycle == lifecycle.Inactive {
		if err := i.externalReferrers(ctx, q, run, st, st.source, summary); err != nil {
			return err
		}
	}

	if row.Deduplicated {
		summary.Deduplicated++
		return nil
	}

	// Only an insertion has a diff. The previous payload comes back on that
	// path alone: without the condition every observation would drag its own
	// payload across the wire while most of them deduplicate and have nothing
	// to compare.
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
	// The comparison runs on the normalized structure, never on the payload as
	// it arrived. The stored side is normalized by construction, so comparing
	// the raw side against it reports every field normalization touches as a
	// change: a version the normalizer drops, a cookie map it turns into names.
	// Two divergent forms reintroduce exactly the false change this comparison
	// exists to remove.
	normalized := obs
	normalized.data = result.Data
	i.diffEvents(run, st, normalized, before, previousVersion, report.Run.Version, st.source, summary)

	// The other direction: this page points at a host the inventory has already
	// declared dead. On the insert path, like the takeover finding, so a render
	// that reports the same page again says nothing rather than re-alerting on
	// every pass for as long as the reference lasts.
	if err := i.externalReferences(ctx, q, run, st, normalized, st.source, summary); err != nil {
		return err
	}

	// A change the HTTP layer detected buys a render, and only in the nominal
	// regime. When the raw client is the one being turned away, the probe keeps
	// running for reachability and for TLS, but what it sees of a target
	// refusing it is not a change worth a browser.
	//
	// A first observation is a first contact rather than a change, and it has
	// its own trigger with its own filter and its own queue. Promoting it here
	// would put every service of a fresh perimeter into the queue that exists
	// to stay short.
	// The same filter the baseline reads. A change on a service Chrome refuses
	// to open would otherwise take the head of the queue and stay there: the
	// pass cannot render it and the promote path had no opinion about that,
	// where the baseline path did.
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

// counter narrows a running total to the column that holds it.
//
// The clamp is not decoration. These counters only ever grow while an asset is
// failing, so the overflow is decades away and would arrive as a negative
// streak that reads as the opposite of what happened.
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

// nextStreak walks the signed counter. It crosses zero rather than
// decrementing, so one success after four failures reads as one success.
func nextStreak(current int, usable bool) int {
	if usable {
		return max(current, 0) + 1
	}
	return min(current, 0) - 1
}

// reschedule moves a host's due dates after a report answered for it.
//
// Only hosts are rescheduled. The target list of a run is a list of hosts, and
// a service is observed through its host's run: giving one its own resolve date
// would put it in a queue nothing dispatches from.
func (i *Ingestor) reschedule(ctx context.Context, q *sqlcgen.Queries, run Run, st *state) error {
	if st.kind != normalize.KindFQDN && st.kind != normalize.KindIP {
		return nil
	}

	at := i.now()
	// An asset that was never alive ends archived rather than inactive. It is
	// not dead: it never existed.
	archive := st.lifecycle == lifecycle.Candidate && lifecycle.Exhausted(st.firstSeen, at)

	// A candidate that answers earns the expensive rung, and this is the only
	// place that can give it one.
	//
	// "resolve, then full once it answers" has a second half, and without it a
	// candidate is created with no full date, checked only ever by resolve runs
	// which leave that date alone, and swept for ports never. The failure is
	// the one movesFull already describes from the other end: silent, total,
	// and invisible to anything that does not look for a null.
	promoted := st.previousLifecycle == lifecycle.Candidate && st.lifecycle != lifecycle.Candidate

	params := sqlcgen.RescheduleAssetParams{
		AssetID: uuidTo(st.id),
		Archive: archive,
		// The tier stored is the one the *next* failure reads. The delay below
		// is the rung this failure earned, which is the tier as it stands.
		// Incrementing first skips the first rung of the curve, and on the
		// candidate curve that rung is one minute and is the whole of the
		// freshness advantage: it is the difference between catching a service
		// as it appears and catching it once somebody has hardened it.
		BackoffTier: counter(lifecycle.NextTier(st.lifecycle, st.tier)),
		MoveResolve: true,
		MoveFull:    run.movesFull() || promoted,
	}
	if !archive {
		resolve := at.Add(i.cadence.Spread(i.cadence.Delay(st.lifecycle, lifecycle.RungResolve, st.tier), i.random()))
		params.NextResolveAt = stamp(resolve)
		if params.MoveFull {
			// A promotion is due now rather than a cadence away: the point of
			// chasing a candidate is to see what it exposes as it appears. It
			// takes the stagger instead of the curve's own spread, because
			// what it is joining is a recurring cadence and a convoy formed
			// there is permanent.
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

// movesFull reports whether this run's scope reached the expensive rung. An
// asset due for full does not need a resolve run, because full runs every rung
// below it, and the reverse is not true.
//
// Only a resolve run leaves the full date alone, and the list used to be the
// other way round: it named the scopes that do sweep and missed the one every
// discovery run carries. The consequence was silent and total, because a
// discovered host is created with no full date at all: nothing would ever have
// swept a discovered host's ports a second time, so a port opened next week was
// invisible, which is the single thing scanning exists for.
func (r Run) movesFull() bool { return r.Scope != lifecycle.RungResolve }
