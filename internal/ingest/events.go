package ingest

import (
	"encoding/json"

	"github.com/JoshuaMart/recon/internal/diff"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/notify"
)

// fieldEvents maps a changed field onto what it is worth telling somebody.
//
// A field nobody listed produces no event of its own. It still travels in the
// diff of whatever event the same observation produced, so nothing is lost: what
// this table decides is what earns a line in somebody's evening, not what is
// recorded.
var fieldEvents = map[string]string{
	"tech":         notify.KindTechChanged,
	"technologies": notify.KindTechChanged,
	"redirects":    notify.KindChainChanged,
	"chain":        notify.KindChainChanged,
	"final_url":    notify.KindChainChanged,
	"tls":          notify.KindCertChanged,
	"title":        notify.KindTitleChanged,
	"open_ports":   notify.KindPortOpened,
}

// notable turns one written observation into the events it is worth.
//
// It runs inside the ingestion transaction, on the two payloads the write
// statement already returned. Both arguments are in hand: re-deriving them in a
// sweep would mean re-parsing what was just parsed, and would miss every
// transient state between two passes.
// announce tells what this observation changed about the asset itself.
//
// It runs on every observation, including a deduplicated one, and that is the
// point. A death is three identical nxdomains: only the first is a new row, and
// the transition happens on the third. Producing transitions only where a row
// was written would make the most common death in the system silent.
func (i *Ingestor) announce(run Run, st *state, obs observation, discoverySource string, summary *Summary) {
	asset := st.id
	at := i.now()

	add := func(kind string, payload map[string]any) {
		event := notify.Event{
			OrgID:     run.OrgID,
			ProgramID: run.ProgramID,
			AssetID:   &asset,
			Kind:      kind,
			Priority:  notify.Priorities[kind],
			Payload:   payload,
		}
		event.Suppressed = summary.grace.Suppresses(kind, discoverySource, at)
		summary.Notifications = append(summary.Notifications, event)
	}

	base := i.base(st, obs)

	// A takeover candidate is the finding this product exists for, and it is
	// never held back by anything.
	if obs.takeover != nil {
		payload := clone(base)
		payload["finding"] = obs.takeover.Map()
		add(notify.KindTakeover, payload)
	}

	// A transition is told once per asset, not once per observation. A host
	// writes a dns layer and a tcp layer in the same report, and both see the
	// same arrival: emitting from each would notify it twice, three times with
	// a service, and every one of them would be true.
	if !st.announced {
		switch {
		case st.lifecycle == lifecycle.Active && st.previousLifecycle != lifecycle.Active:
			payload := clone(base)
			payload["from"] = st.previousLifecycle
			// The layer says nothing about an arrival, and naming the one that
			// happened to be written first would be arbitrary.
			delete(payload, "layer")
			add(notify.KindNewActive, payload)
			st.announced = true
		case st.lifecycle == lifecycle.Inactive && st.previousLifecycle != lifecycle.Inactive:
			payload := clone(base)
			payload["from"] = st.previousLifecycle
			delete(payload, "layer")
			add(notify.KindWentInactive, payload)
			st.announced = true
		}
	}
}

// diffEvents tells what this observation changed about the world.
//
// It runs only where a row was written, because that is the only path with two
// payloads to compare: a deduplicated observation is the same state seen again.
func (i *Ingestor) diffEvents(
	run Run, st *state, obs observation, previous map[string]any,
	previousVersion, version, discoverySource string, summary *Summary,
) {
	changes := diff.Compare(previous, obs.data)
	if len(changes) == 0 {
		return
	}

	asset := st.id
	at := i.now()
	add := func(kind string, payload map[string]any) {
		event := notify.Event{
			OrgID:     run.OrgID,
			ProgramID: run.ProgramID,
			AssetID:   &asset,
			Kind:      kind,
			Priority:  notify.Priorities[kind],
			Payload:   payload,
		}
		event.Suppressed = summary.grace.Suppresses(kind, discoverySource, at)
		summary.Notifications = append(summary.Notifications, event)
	}
	base := i.base(st, obs)

	// The instrument is dated. A pure addition across a version bump is the
	// observer seeing better rather than the world changing, and untreated it
	// would alert across a whole inventory after one update.
	if diff.Revelation(changes, previousVersion, version) {
		payload := clone(base)
		payload["diff"] = changes
		payload["summary"] = diff.Summarise(changes)
		payload["from_version"] = previousVersion
		payload["to_version"] = version
		add(notify.KindDetection, payload)
		return
	}

	// One event per kind of change rather than one per changed field, so a
	// deployment that moves four things is one line and not four.
	byKind := map[string][]diff.Change{}
	for _, change := range changes {
		kind, listed := fieldEvents[change.Field]
		if !listed {
			continue
		}
		// A port list that only lost members is not a port opening. It is the
		// host closing something, which the lifecycle already speaks for.
		if kind == notify.KindPortOpened && len(change.Added) == 0 {
			continue
		}
		byKind[kind] = append(byKind[kind], change)
	}

	for _, kind := range order {
		grouped, present := byKind[kind]
		if !present {
			continue
		}
		payload := clone(base)
		payload["diff"] = grouped
		payload["summary"] = diff.Summarise(grouped)
		add(kind, payload)
	}
}

// base is what every event of this observation carries.
func (i *Ingestor) base(st *state, obs observation) map[string]any {
	base := map[string]any{
		"key":       st.key,
		"layer":     string(obs.layer),
		"lifecycle": st.lifecycle,
	}
	if len(st.lineage) > 0 {
		// Every notification carries the lineage, not just the current state.
		// It is in the row the upsert just wrote, so it costs nothing here.
		base["lineage"] = jsonAny(json.RawMessage(st.lineage))
	}
	return base
}

// order fixes the sequence events are produced in, so a batch reads the same
// way twice and a test can assert one.
var order = []string{
	notify.KindPortOpened,
	notify.KindTechChanged,
	notify.KindChainChanged,
	notify.KindCertChanged,
	notify.KindTitleChanged,
}

func clone(base map[string]any) map[string]any {
	out := make(map[string]any, len(base)+3)
	for key, value := range base {
		out[key] = value
	}
	return out
}
