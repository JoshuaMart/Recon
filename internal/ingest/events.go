package ingest

import (
	"encoding/json"

	"github.com/JoshuaMart/recon/internal/diff"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/notify"
)

// fieldEvents maps a changed field onto what is worth telling someone. An unlisted
// field makes no event of its own but still travels in the diff of whatever event
// the same observation produced: this table decides what earns a line, not what
// is recorded.
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

// announce tells what this observation changed about the asset itself. It runs on
// every observation, deduplicated ones included: a death is three identical
// nxdomains, only the first a new row, and the transition lands on the third.
// Producing transitions only on written rows would silence the most common death.
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

	// A transition is told once per asset, not per observation: a host writes a dns
	// and a tcp layer in one report, and emitting from each would notify the same
	// arrival two or three times, each true.
	if st.announced != st.lifecycle {
		switch {
		case st.lifecycle == lifecycle.Active && st.previousLifecycle != lifecycle.Active:
			payload := clone(base)
			payload["from"] = st.previousLifecycle
			// The layer says nothing about an arrival, and naming the one written first would
			// be arbitrary.
			delete(payload, "layer")
			add(notify.KindNewActive, payload)
			st.announced = st.lifecycle
		case st.lifecycle == lifecycle.Inactive && st.previousLifecycle != lifecycle.Inactive:
			payload := clone(base)
			payload["from"] = st.previousLifecycle
			delete(payload, "layer")
			add(notify.KindWentInactive, payload)
			st.announced = st.lifecycle
		}
	}
}

// diffEvents tells what this observation changed about the world. Only where a row
// was written, the only path with two payloads to compare: a deduplicated
// observation is the same state seen again.
func (i *Ingestor) diffEvents(
	run Run, st *state, obs observation, previous map[string]any,
	previousVersion, version, discoverySource string, summary *Summary,
) {
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

	// A takeover candidate is the finding this product exists for and nothing holds it
	// back. It rides the insert path, not the transition one: a dangling name is
	// re-derived from every report and critical escapes the windows, so announcing it
	// would re-send the alert every scan. A re-confirmed finding deduplicates silently.
	if obs.takeover != nil {
		payload := clone(base)
		payload["finding"] = obs.takeover.Map()
		add(notify.KindTakeover, payload)
	}

	changes := diff.Compare(previous, obs.data)
	if len(changes) == 0 {
		return
	}

	// The instrument is dated. A pure addition across a version bump is the observer
	// seeing better, and would alert across a whole inventory after one update.
	//
	// Only on the layer the classification is about: the scanner version is stamped on
	// dns, tcp and http too, so applying it there would reclassify a newly opened port
	// as a detection improvement for a pass after every scanner upgrade.
	if obs.layer == normalize.LayerFingerprint && diff.Revelation(changes, previousVersion, version) {
		payload := clone(base)
		payload["diff"] = changes
		payload["summary"] = diff.Summarise(changes)
		payload["from_version"] = previousVersion
		payload["to_version"] = version
		add(notify.KindDetection, payload)
		return
	}

	// One event per kind of change, not per field: four changes are one line.
	byKind := map[string][]diff.Change{}
	for _, change := range changes {
		kind, listed := fieldEvents[change.Field]
		if !listed {
			continue
		}
		// A port list that only lost members is not a port opening; it is the host closing
		// something, which the lifecycle already reports.
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
		// Every notification carries the lineage, free from the row the upsert wrote.
		base["lineage"] = jsonAny(json.RawMessage(st.lineage))
	}
	return base
}

// order fixes the production sequence so a batch reads the same twice.
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
