//go:build integration

package ingest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ingest"
)

func (h *harness) candidateRun() ingest.Run {
	return ingest.Run{
		ID: uuid.New(), OrgID: h.org, ProgramID: h.program,
		Source: ingest.SourceCertstream,
		Certificate: &ingest.Certificate{
			Issuer: "Let's Encrypt",
			Log:    "Google 'PlumbersArms2026h2' log",
			Index:  942067477,
		},
	}
}

func (h *harness) lineageOf(t *testing.T, key string) map[string]any {
	t.Helper()

	var raw []byte
	err := h.pool.QueryRow(context.Background(),
		`SELECT discovery_path FROM asset WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&raw)
	if err != nil {
		t.Fatalf("read the lineage of %s: %v", key, err)
	}
	var steps []map[string]any
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode the lineage of %s: %v", key, err)
	}
	if len(steps) == 0 {
		t.Fatalf("the lineage of %s is empty", key)
	}
	return steps[0]
}

func (h *harness) sourceOf(t *testing.T, key string) string {
	t.Helper()

	var source string
	err := h.pool.QueryRow(context.Background(),
		`SELECT discovery_source FROM asset WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&source)
	if err != nil {
		t.Fatalf("read the source of %s: %v", key, err)
	}
	return source
}

// The rung is the whole point. Nobody typed this name in, a log did, and most
// candidates never resolve to anything: the cheap rung answers whether the name
// exists yet, and the expensive one is earned by an answer.
func TestACandidateIsDueForResolveAndNotForFull(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}

	entered, err := h.dated(c).EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{"staging.acme.test", "not a host"})
	if err != nil {
		t.Fatalf("enter candidates: %v", err)
	}
	if len(entered.Accepted) != 1 || len(entered.Refused) != 1 {
		t.Fatalf("%d accepted and %d refused", len(entered.Accepted), len(entered.Refused))
	}
	if !entered.Accepted[0].Scheduled {
		t.Error("the candidate reports itself unscheduled, and a caller reads that to know " +
			"whether anything will go and look")
	}

	// Immediate and with no jitter: the certificate is the event, and the
	// aggressive curve rests on the first check happening now.
	if due := h.dueOf(t, "staging.acme.test"); !due.Equal(c.now) {
		t.Errorf("the resolve due date is %s, want %s", due, c.now)
	}

	var full *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_full_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, "staging.acme.test").Scan(&full); err != nil {
		t.Fatalf("read the full due date: %v", err)
	}
	if full != nil {
		t.Errorf("a candidate was scheduled for a full run at %s, and a hundred connections per "+
			"host is not what answers whether a name exists yet", full)
	}

	if got := h.lifecycleOf(t, "staging.acme.test"); got != "candidate" {
		t.Errorf("the lifecycle is %q, and it was discovered and never verified alive", got)
	}
}

// "Why is this here" has to be answerable six months later by somebody looking
// at a name they do not recognise, and it is the reason the lite stream is
// dialled rather than the one carrying names alone.
func TestACandidateCarriesItsCertificateInItsLineage(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}

	if _, err := h.dated(c).EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{"staging.acme.test"}); err != nil {
		t.Fatalf("enter candidates: %v", err)
	}

	if got := h.sourceOf(t, "staging.acme.test"); got != ingest.SourceCertstream {
		t.Errorf("the discovery source is %q, want %q", got, ingest.SourceCertstream)
	}

	step := h.lineageOf(t, "staging.acme.test")
	if step["step"] != "certificate_transparency" {
		t.Errorf("the lineage step is %v", step["step"])
	}
	if step["issuer"] != "Let's Encrypt" {
		t.Errorf("the lineage carries issuer %v", step["issuer"])
	}
	if step["log"] != "Google 'PlumbersArms2026h2' log" {
		t.Errorf("the lineage carries log %v", step["log"])
	}
	if index, ok := step["cert_index"].(float64); !ok || int64(index) != 942067477 {
		t.Errorf("the lineage carries cert_index %v", step["cert_index"])
	}
}

// CT classifies, it does not filter. Filtering in the loop would be a second
// scope engine, and two engines disagree eventually.
func TestACandidateOutsideThePerimeterIsStoredAndNeverProbed(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"), exclude("fqdn", "internal.acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}

	entered, err := h.dated(c).EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{"internal.acme.test", "elsewhere.example.org"})
	if err != nil {
		t.Fatalf("enter candidates: %v", err)
	}
	if len(entered.Accepted) != 2 {
		t.Fatalf("%d of 2 names were stored", len(entered.Accepted))
	}

	for _, key := range []string{"internal.acme.test", "elsewhere.example.org"} {
		var resolve, full *time.Time
		if err := h.pool.QueryRow(context.Background(),
			`SELECT next_resolve_at, next_full_at FROM asset_current
			  WHERE program_id = $1 AND key = $2`,
			h.program, key).Scan(&resolve, &full); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if resolve != nil || full != nil {
			t.Errorf("%s carries a due date, and it is outside the perimeter", key)
		}
	}

	for _, accepted := range entered.Accepted {
		if accepted.Scheduled {
			t.Errorf("%s reports itself scheduled while carrying no due date", accepted.Key)
		}
	}
}

// A widening of rediscovery, and it is the case the freshness advantage exists
// for: a name this system gave up on, appearing in a fresh certificate, is the
// strongest available signal that somebody is provisioning it again.
func TestACertificateForAnArchivedNameBringsItBack(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "gaveup.acme.test"
	if _, err := ing.EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{name}); err != nil {
		t.Fatalf("enter candidates: %v", err)
	}

	// Chased until the budget ran out: out of the scheduler, no due date, and
	// never INACTIVE because it never existed.
	exec(t, h.pool,
		`UPDATE asset_current SET lifecycle = 'archived', next_resolve_at = NULL, next_full_at = NULL
		  WHERE program_id = $1 AND key = $2`, h.program, name)
	if got := h.lifecycleOf(t, name); got != "archived" {
		t.Fatalf("the fixture left the asset %q", got)
	}

	later := c.now.Add(90 * 24 * time.Hour)
	if _, err := h.dated(&clock{now: later}).EnterCandidates(
		context.Background(), h.queries, h.candidateRun(), set, []string{name}); err != nil {
		t.Fatalf("re-enter the candidate: %v", err)
	}

	if due := h.dueOf(t, name); !due.Equal(later) {
		t.Errorf("the revived candidate is due at %s, want %s", due, later)
	}
	if got := h.lifecycleOf(t, name); got == "archived" {
		t.Error("a fresh certificate left the name archived, so the freshness advantage was " +
			"thrown away at the moment it is worth most")
	}
}

// The second half of "resolve, then full once it answers", and without it a
// candidate is checked only ever by resolve runs, which leave the full date
// alone, and swept for ports never.
func TestACandidateThatAnswersEarnsTheExpensiveRung(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "live.acme.test"
	if _, err := ing.EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{name}); err != nil {
		t.Fatalf("enter candidates: %v", err)
	}

	// A candidate run is pinned to resolve, which is exactly the scope that
	// leaves the full date alone everywhere else.
	resolve := h.run()
	resolve.Kind = "candidate"
	resolve.Scope = "resolve"
	resolve.Targets = map[string]struct{}{name: {}}

	c.now = c.now.Add(time.Minute)
	if _, err := ing.Report(context.Background(), h.queries, resolve, set,
		liveHost(name)); err != nil {
		t.Fatalf("ingest the resolve report: %v", err)
	}

	if got := h.lifecycleOf(t, name); got != "active" {
		t.Fatalf("the candidate answered and is %q", got)
	}

	var full *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_full_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, name).Scan(&full); err != nil {
		t.Fatalf("read the full due date: %v", err)
	}
	if full == nil {
		t.Fatal("a candidate that answered carries no full due date, so nothing will ever " +
			"sweep its ports and the port it opened is invisible")
	}
	// Due now rather than a cadence away: the point of chasing a candidate is
	// to see what it exposes as it appears.
	if full.Before(c.now) || full.After(c.now.Add(time.Hour)) {
		t.Errorf("the promoted full date is %s, and the candidate answered at %s", full, c.now)
	}
}

// The control. A host that was never a candidate must not be promoted by a
// resolve run, or every resolve pass would drag the whole inventory onto the
// expensive rung.
func TestAResolveRunStillLeavesAnActiveHostsFullDateAlone(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "known.acme.test"
	full := h.run()
	full.Scope = "full"
	if _, err := ing.Report(context.Background(), h.queries, full, set, liveHost(name)); err != nil {
		t.Fatalf("ingest the full report: %v", err)
	}
	before := h.fullDueOf(t, name)

	c.now = c.now.Add(time.Hour)
	resolve := h.run()
	resolve.Scope = "resolve"
	resolve.Targets = map[string]struct{}{name: {}}
	if _, err := ing.Report(context.Background(), h.queries, resolve, set, liveHost(name)); err != nil {
		t.Fatalf("ingest the resolve report: %v", err)
	}

	if after := h.fullDueOf(t, name); !after.Equal(before) {
		t.Errorf("a resolve run moved an active host's full date from %s to %s", before, after)
	}
}

// A candidate that was never reachable ends ARCHIVED and not INACTIVE. It is
// not dead: it never existed, and the two readings call for opposite things in
// a console.
func TestACandidateThatIsNeverReachableEndsArchived(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "never.acme.test"
	if _, err := ing.EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{name}); err != nil {
		t.Fatalf("enter candidates: %v", err)
	}

	// Chased on the curve, answering nothing, for longer than the budget. The
	// failures are informative on purpose: nxdomain with resolver consensus is
	// the strongest death signal there is, and even that must not make a name
	// that never existed read as one that died.
	resolve := h.run()
	resolve.Kind = "candidate"
	resolve.Scope = "resolve"
	resolve.Targets = map[string]struct{}{name: {}}

	gone := deadHost(name, ingest.ReasonNXDomain)
	for range 8 {
		if _, err := ing.Report(context.Background(), h.queries, resolve, set, gone); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		c.now = c.now.Add(3 * 24 * time.Hour)
	}

	if got := h.lifecycleOf(t, name); got != "archived" {
		t.Fatalf("the candidate ended %q, and a name whose infrastructure was never provisioned "+
			"is not dead: it never existed", got)
	}

	// Out of the scheduler, so nothing selects it and no lane keeps paying for
	// it.
	var resolveDue, fullDue *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_resolve_at, next_full_at FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, name).Scan(&resolveDue, &fullDue); err != nil {
		t.Fatalf("read the due dates: %v", err)
	}
	if resolveDue != nil || fullDue != nil {
		t.Errorf("an archived candidate still carries due dates: resolve %v, full %v",
			resolveDue, fullDue)
	}
}

// What Certificate Transparency notifies, and when.
//
// Nothing on the certificate. A candidate is not a finding: most of them never
// resolve to anything, and notifying on creation would produce the exact flood
// the anti-flood exists to stop, on the one source that can deliver several
// thousand names in a minute.
//
// The event is that one went live, and it is new_active, which the table of 12.2
// already carried before the source that produces it existed.
func TestACandidateNotifiesNothingUntilItGoesLive(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "arriving.acme.test"
	if _, err := ing.EnterCandidates(context.Background(), h.queries, h.candidateRun(), set,
		[]string{name}); err != nil {
		t.Fatalf("enter candidates: %v", err)
	}

	if events := h.events(t); len(events) != 0 {
		t.Fatalf("a certificate produced %d notifications, and a candidate is not a finding: %+v",
			len(events), events)
	}

	// The first check answers, on the cheap rung, from the candidate lane.
	resolve := h.run()
	resolve.Kind = "candidate"
	resolve.Scope = "resolve"
	resolve.Targets = map[string]struct{}{name: {}}

	c.now = c.now.Add(time.Minute)
	if _, err := ing.Report(context.Background(), h.queries, resolve, set, liveHost(name)); err != nil {
		t.Fatalf("ingest the resolve report: %v", err)
	}

	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("%d notifications when the candidate went live: %+v", len(events), events)
	}
	arrival := events[0]
	if arrival.Kind != "new_active" {
		t.Errorf("the event is %q, and phase 8 adds no kind of its own", arrival.Kind)
	}
	if arrival.Key != name {
		t.Errorf("the event names %q", arrival.Key)
	}
	// The programme has never had a discovery run, so the grace does not hold
	// this back: an asset a log found under an apex was typed in by nobody.
	if arrival.Suppressed {
		t.Error("the arrival was suppressed, and it is the freshness advantage arriving")
	}
	if from, _ := arrival.Payload["from"].(string); from != "candidate" {
		t.Errorf("the event says it came from %q", from)
	}

	// And it is told once. A host writes a dns layer and a tcp layer in the
	// same report, and both see the same arrival.
	c.now = c.now.Add(time.Hour)
	if _, err := ing.Report(context.Background(), h.queries, resolve, set, liveHost(name)); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if events := h.events(t); len(events) != 1 {
		t.Errorf("%d notifications after a second report that changed nothing", len(events))
	}
}
