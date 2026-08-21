package ingest

import (
	"context"
	"fmt"

	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// referenceCap bounds how many referencing assets one death reports.
//
// A widely referenced internal host is exactly the case that matters and
// exactly the one that would produce a thousand critical alerts. The cap is
// stated in the payload rather than applied in silence.
const referenceCap = 25

// externalReferences is the internal half of external_host_dead, read from the
// referencing side.
//
// On the insert path only, which is the same reasoning that puts the takeover
// finding there: a render that reports the same page again says nothing, so a
// script pointing at a host that has been dead for a month does not re-alert on
// every pass. The other direction, the host dying after the render, is caught
// by the transition rather than by this.
func (i *Ingestor) externalReferences(
	ctx context.Context, q *sqlcgen.Queries, run Run, st *state, obs observation,
	discoverySource string, summary *Summary,
) error {
	if obs.layer != normalize.LayerFingerprint {
		return nil
	}
	hosts := textList(obs.data["external_hosts"])
	if len(hosts) == 0 {
		return nil
	}

	dead, err := q.DeadExternalHosts(ctx, sqlcgen.DeadExternalHostsParams{
		OrgID: uuidTo(run.OrgID),
		Hosts: hosts,
	})
	if err != nil {
		return fmt.Errorf("cross-reference the external hosts of %s: %w", st.id, err)
	}
	for _, host := range dead {
		if host == nil {
			continue
		}
		i.emitExternalDead(run, st, *host, []string{st.key}, false, discoverySource, summary)
	}
	return nil
}

// externalReferrers is the same finding read from the dying side.
//
// It runs on the transition into inactive, which is the only moment the fact
// exists: nothing in a payload comparison says that somebody else's page points
// at this name.
func (i *Ingestor) externalReferrers(
	ctx context.Context, q *sqlcgen.Queries, run Run, st *state,
	discoverySource string, summary *Summary,
) error {
	// Only a name can be pointed at by a script. A service is a name and a
	// port, and nothing writes "cdn.example.test:443/tcp" into a src attribute.
	if st.kind != normalize.KindFQDN {
		return nil
	}

	rows, err := q.ReferencesToHost(ctx, sqlcgen.ReferencesToHostParams{
		OrgID: uuidTo(run.OrgID),
		Host:  st.key,
		Cap:   referenceCap + 1,
	})
	if err != nil {
		return fmt.Errorf("find what references %s: %w", st.key, err)
	}
	if len(rows) == 0 {
		return nil
	}

	cut := false
	if len(rows) > referenceCap {
		rows = rows[:referenceCap]
		cut = true
	}
	referrers := make([]string, 0, len(rows))
	for _, row := range rows {
		referrers = append(referrers, row.Key)
	}
	i.emitExternalDead(run, st, st.key, referrers, cut, discoverySource, summary)
	return nil
}

// emitExternalDead writes the event both directions produce.
//
// One shape whichever side noticed, because the finding is the same one and a
// console that had to tell two apart would be reading the mechanism rather than
// the fact.
func (i *Ingestor) emitExternalDead(
	run Run, st *state, host string, referrers []string, cut bool,
	discoverySource string, summary *Summary,
) {
	asset := st.id
	payload := map[string]any{
		"key":           st.key,
		"external_host": host,
		"referenced_by": referrers,
		"summary":       fmt.Sprintf("%s is dead and %d asset(s) still point at it", host, len(referrers)),
	}
	if cut {
		// A cap is said rather than applied in silence, which is the same rule
		// the export and the timeline follow.
		payload["truncated"] = true
	}

	event := notify.Event{
		OrgID:     run.OrgID,
		ProgramID: run.ProgramID,
		AssetID:   &asset,
		Kind:      notify.KindExternalDead,
		Priority:  notify.Priorities[notify.KindExternalDead],
		Payload:   payload,
	}
	event.Suppressed = summary.grace.Suppresses(event.Kind, discoverySource, i.now())
	summary.Notifications = append(summary.Notifications, event)
}
