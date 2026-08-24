//go:build integration

package runs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/runs"
)

// candidates inserts names on the candidate curve, due now on resolve alone.
// That is exactly what EnterCandidates writes: no full date, because the cheap
// rung answers the only question worth asking first.
func (h *harness) candidates(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		id := uuid.New()
		exec(t, h.pool, `INSERT INTO asset
			(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,$4,'certstream','in_scope',$5,$5)`,
			id, h.org, h.program, name, h.clock.now.Add(-time.Minute))
		exec(t, h.pool, `INSERT INTO asset_current
			(asset_id, org_id, program_id, kind, key, scope_status, host, lifecycle,
			 next_resolve_at, first_seen, last_seen)
			VALUES ($1,$2,$3,'fqdn',$4,'in_scope',$4,'candidate',$5,$5,$5)`,
			id, h.org, h.program, name, h.clock.now.Add(-time.Minute))
	}
}

func (h *harness) pass(t *testing.T, platform *recorder) *runs.CandidatePass {
	t.Helper()

	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(platform))
	return runs.NewCandidatePass(h.pool, scheduler, time.Minute, quiet())
}

// The milestone assertion, and what answers it is the input rather than the
// length of the list: stage 1 is replaced, so no enumeration runs and no source
// quota is spent whatever the list holds.
func TestACandidatesFirstCheckRunsNoEnumerationAndSpendsNoQuota(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.candidates(t, "fresh.acme.test")

	platform := &recorder{id: "run-candidate"}
	started, err := h.pass(t, platform).Once(ctx)
	if err != nil {
		t.Fatalf("candidate pass: %v", err)
	}
	if started != 1 {
		t.Fatalf("%d runs went out", started)
	}

	var kind, scope string
	var targets int
	var deadline, created time.Time
	if err := h.pool.QueryRow(ctx, `
		SELECT r.kind, r.scope, count(t.asset_id)::int, r.deadline, r.created_at
		  FROM run r LEFT JOIN run_target t ON t.run_id = r.id
		 WHERE r.program_id = $1 GROUP BY r.id`, h.program).
		Scan(&kind, &scope, &targets, &deadline, &created); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if kind != runs.KindCandidate {
		t.Errorf("kind = %s, want %s", kind, runs.KindCandidate)
	}
	// Pinned, and by a CHECK rather than by this pass being careful. A full run
	// on this lane would be a hundred connections per host beside a sweep that
	// already holds the programme's budget.
	if scope != "resolve" {
		t.Errorf("scope = %s: nothing on this lane may reach the expensive rung", scope)
	}
	if targets != 1 {
		t.Errorf("the run froze %d targets", targets)
	}

	// A slot held for a sweep's budget by a run that had one thing to do turns
	// the bound this lane exists for back into the problem it solves.
	if held := deadline.Sub(created); held >= h.sched.Config().Timeout {
		t.Errorf("the candidate run holds its slot for %s, as long as a sweep", held)
	}

	// The targets input is what makes the first check cheap, and the flags are
	// where that is visible: a list replaces stage 1, so no source is queried.
	var domain, stages string
	for i, arg := range platform.args {
		switch arg {
		case "--domain", "-d":
			if i+1 < len(platform.args) {
				domain = platform.args[i+1]
			}
		case "--stages":
			if i+1 < len(platform.args) {
				stages = platform.args[i+1]
			}
		}
	}
	if domain != "" {
		t.Errorf("the candidate run was given --domain %q, which is the enumeration input", domain)
	}
	if stages != "resolve" {
		t.Errorf("--stages %q", stages)
	}
	if !hasFlag(platform.args, "--targets-url") {
		t.Error("the candidate run carries no targets URL, so stage 1 would query the sources")
	}
}

// The whole reason the lane is a third kind rather than a flag on the second.
func TestACandidateRunAndAVerificationRunCoexistButTwoCandidatesDoNot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.due(t, "known.acme.test")
	h.candidates(t, "fresh.acme.test")

	// The sweep first, holding the programme's verification slot.
	sweep := &recorder{id: "run-verify"}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(sweep))
	if started, err := runs.NewDuePass(h.pool, scheduler, time.Minute, quiet()).Once(ctx); err != nil {
		t.Fatalf("due pass: %v", err)
	} else if started != 1 {
		t.Fatalf("%d verification runs went out", started)
	}

	// And the candidate goes out beside it. On the due date pass it would have
	// waited for the sweep's whole deadline.
	platform := &recorder{id: "run-candidate"}
	pass := h.pass(t, platform)
	if started, err := pass.Once(ctx); err != nil {
		t.Fatalf("candidate pass: %v", err)
	} else if started != 1 {
		t.Fatalf("%d candidate runs went out while a sweep was in flight, and the whole "+
			"point of the lane is that it does not wait for one", started)
	}

	// A second one does not, and the index is what says so rather than a check.
	h.candidates(t, "another.acme.test")
	if started, err := pass.Once(ctx); err != nil {
		t.Fatalf("second candidate pass: %v", err)
	} else if started != 0 {
		t.Errorf("%d candidate runs went out with one already in flight", started)
	}

	var kinds int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(DISTINCT kind)::int FROM run WHERE program_id = $1 AND state IN ('pending','running')`,
		h.program).Scan(&kinds); err != nil {
		t.Fatalf("read the live runs: %v", err)
	}
	if kinds != 2 {
		t.Errorf("%d kinds of run are in flight, want a verification and a candidate", kinds)
	}
}

// The exclusion is mutual, and without it the two passes fight over the same
// names: each freezes what the other was about to take.
func TestTheTwoPassesDoNotTakeEachOthersNames(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.candidates(t, "fresh.acme.test")

	// No ordinary due asset at all, so the due date pass has nothing of its own.
	sweep := &recorder{id: "run-verify"}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(sweep))
	started, err := runs.NewDuePass(h.pool, scheduler, time.Minute, quiet()).Once(ctx)
	if err != nil {
		t.Fatalf("due pass: %v", err)
	}
	if started != 0 {
		t.Fatalf("the due date pass took %d candidates, and they belong to the other lane", started)
	}

	// And the candidate is still there for the pass that owns it.
	platform := &recorder{id: "run-candidate"}
	if started, err := h.pass(t, platform).Once(ctx); err != nil {
		t.Fatalf("candidate pass: %v", err)
	} else if started != 1 {
		t.Fatalf("%d candidate runs went out", started)
	}
}

// The lease is one lease across the lanes rather than one per lane, or a host
// would be scanned twice at once by two runs that cannot see each other.
func TestACandidateHeldByAVerificationRunIsNotTakenAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.candidates(t, "held.acme.test")

	// Freeze it by hand into a live verification run, which is the state a
	// reclassification can produce: the name was ordinary when the sweep took
	// it and is a candidate now.
	runID := uuid.New()
	exec(t, h.pool, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
		VALUES ($1,$2,$3,'verification','full','running',$4)`,
		runID, h.org, h.program, h.clock.now.Add(time.Hour))
	exec(t, h.pool, `INSERT INTO run_target (run_id, asset_id, org_id, key)
		SELECT $1, asset_id, org_id, key FROM asset_current WHERE program_id = $2`,
		runID, h.program)

	platform := &recorder{id: "run-candidate"}
	started, err := h.pass(t, platform).Once(ctx)
	if err != nil {
		t.Fatalf("candidate pass: %v", err)
	}
	if started != 0 {
		t.Errorf("%d candidate runs took a host a live run already holds", started)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// The correction that defines the lane, asserted rather than commented.
//
// CANDIDATE is not a Certificate Transparency state. A host somebody typed into
// the assets form is one too until its first answer, and it carries a full date
// because they typed it in to find out what it exposes. Defining the lane on the
// lifecycle alone would divert it into a pass pinned to resolve, where a
// resolution reports that the name answers and nothing ever sweeps a port.
func TestAHandEnteredCandidateStaysWithTheDueDatePassAndGetsFull(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// What the assets form writes: candidate, due on both rungs at once.
	id := uuid.New()
	exec(t, h.pool, `INSERT INTO asset
		(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1,$2,$3,'fqdn',$4,$4,'manual','in_scope',$5,$5)`,
		id, h.org, h.program, "typed.acme.test", h.clock.now.Add(-time.Minute))
	exec(t, h.pool, `INSERT INTO asset_current
		(asset_id, org_id, program_id, kind, key, scope_status, host, lifecycle,
		 next_resolve_at, next_full_at, first_seen, last_seen)
		VALUES ($1,$2,$3,'fqdn',$4,'in_scope',$4,'candidate',$5,$5,$5,$5)`,
		id, h.org, h.program, "typed.acme.test", h.clock.now.Add(-time.Minute))

	// The candidate lane must not take it.
	platform := &recorder{id: "run-candidate"}
	if started, err := h.pass(t, platform).Once(ctx); err != nil {
		t.Fatalf("candidate pass: %v", err)
	} else if started != 0 {
		t.Fatalf("the candidate lane took a hand entered host, and it would only ever resolve it")
	}

	// And the due date pass gives it the expensive rung, which is what it is
	// promised because a person is waiting.
	sweep := &recorder{id: "run-verify"}
	scheduler := runs.New(h.sched.Signer(), h.sched.Config(), quiet(),
		runs.WithClock(h.clock.Now), runs.WithPlatform(sweep))
	if started, err := runs.NewDuePass(h.pool, scheduler, time.Minute, quiet()).Once(ctx); err != nil {
		t.Fatalf("due pass: %v", err)
	} else if started != 1 {
		t.Fatalf("%d verification runs went out for a hand entered host", started)
	}

	var kind, scope string
	if err := h.pool.QueryRow(ctx,
		`SELECT kind, scope FROM run WHERE program_id = $1`, h.program).Scan(&kind, &scope); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if kind != "verification" || scope != "full" {
		t.Errorf("the run is a %s on %s, and a typed name is due for full", kind, scope)
	}
}
