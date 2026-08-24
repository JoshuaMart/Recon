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
