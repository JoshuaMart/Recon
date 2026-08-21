//go:build integration

// The two render triggers that are API entry points. They hold manage_jobs
// rather than ingest, and the distinction is not tidiness: something holding
// ingest could otherwise schedule renders of its choosing and spend a
// programme's budget on targets it picked.
package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/lifecycle"
)

// renderable writes a service already queued for a render, far out.
func (h *harness) renderable(t *testing.T, key string, due time.Time, priority int16) uuid.UUID {
	t.Helper()

	id := uuid.New()
	h.exec(t, `INSERT INTO asset
		(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1,$2,$3,'service',$4,'app.target.test','fixture','in_scope', now(), now())`,
		id, h.org, h.program, key)
	h.exec(t, `INSERT INTO asset_current
		(asset_id, org_id, program_id, kind, key, scope_status, host, port, scheme,
		 lifecycle, next_fingerprint_at, fingerprint_priority, first_seen, last_seen)
		VALUES ($1,$2,$3,'service',$4,'in_scope','app.target.test',443,'https',
		        'active',$5,$6, now(), now())`,
		id, h.org, h.program, key, due, priority)
	return id
}

func (h *harness) render(t *testing.T, id uuid.UUID) (time.Time, int16) {
	t.Helper()

	var due time.Time
	var priority int16
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_fingerprint_at, fingerprint_priority FROM asset_current WHERE asset_id = $1`,
		id).Scan(&due, &priority); err != nil {
		t.Fatalf("read the render schedule: %v", err)
	}
	return due, priority
}

func TestAManualRequestPutsOneAssetAtTheHead(t *testing.T) {
	h := newHarness(t)
	later := time.Now().Add(20 * 24 * time.Hour)
	id := h.renderable(t, "app.target.test:443/tcp", later, lifecycle.PriorityBaseline)

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", jobs, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a manual request answered %s: %v", resp.Status, body)
	}
	if body["queued"] != true {
		t.Fatalf("the answer reads %v", body)
	}

	due, priority := h.render(t, id)
	if !due.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("the render is still at %s", due)
	}
	if priority != lifecycle.PriorityChange {
		t.Fatalf("a manual request entered at priority %d, behind every baseline of a fresh perimeter", priority)
	}

	// The scope action does not hold it. A credential that can widen the
	// perimeter is a different privilege from one that can spend the budget.
	scopes := h.token(t, h.org, auth.ActionManageScope)
	refused, _ := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", scopes, nil)
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("a scope credential requested a render: %s", refused.Status)
	}

	// And an asset of another organization does not exist.
	stranger := uuid.New()
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'other')`, stranger)
	elsewhere := h.token(t, stranger, auth.ActionManageJobs)
	missing, _ := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", elsewhere, nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("another organization's asset answered %s", missing.Status)
	}
}

// An asset that has left the scheduler is not queued, and the answer says so
// rather than claiming a render that will never be selected.
func TestAManualRequestOnAnArchivedAssetClaimsNothing(t *testing.T) {
	h := newHarness(t)
	id := h.renderable(t, "app.target.test:443/tcp", time.Now().Add(time.Hour), lifecycle.PriorityBaseline)
	h.exec(t, `UPDATE asset_current SET lifecycle = 'archived' WHERE asset_id = $1`, id)

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", jobs, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the request answered %s", resp.Status)
	}
	if body["queued"] != false {
		t.Fatalf("an archived asset was reported as queued: %v", body)
	}
}

// The forced refresh after a major update of the rendering service. The whole
// inventory goes back into the low queue, spread over several days: doing it in
// an hour would be the mass alert the spread exists to avoid.
func TestAReplanBringsRendersForwardAndNeverDelaysOne(t *testing.T) {
	h := newHarness(t)

	far := time.Now().Add(20 * 24 * time.Hour)
	soon := time.Now().Add(time.Minute)
	stale := h.renderable(t, "app.target.test:8080/tcp", far, lifecycle.PriorityBaseline)
	urgent := h.renderable(t, "app.target.test:8443/tcp", soon, lifecycle.PriorityChange)

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/renders/replan", jobs, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the replan answered %s: %v", resp.Status, body)
	}
	if body["replanned"] != float64(2) {
		t.Fatalf("the replan reports %v assets", body["replanned"])
	}

	moved, movedPriority := h.render(t, stale)
	if !moved.Before(far) {
		t.Fatalf("a stale render stayed at %s", moved)
	}
	if !moved.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("the replan put a render in the past at %s, so the whole inventory is due at once", moved)
	}
	if movedPriority != lifecycle.PriorityBaseline {
		t.Fatalf("the replan raised a priority to %d", movedPriority)
	}

	// It may only bring a render forward. A manual request or a detected change
	// lives in the high queue with an immediate due date, and a replan
	// triggered a second later would bury it for a week without trace.
	kept, keptPriority := h.render(t, urgent)
	if kept.After(soon.Add(time.Second)) {
		t.Fatalf("the replan delayed an urgent render from %s to %s", soon, kept)
	}
	if keptPriority != lifecycle.PriorityChange {
		t.Fatalf("the replan demoted an urgent render to %d", keptPriority)
	}

	// And it leaves untouched what has left the scheduler.
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE org_id = $1 AND next_fingerprint_at IS NULL`,
		h.org); n != 0 {
		t.Fatalf("%d assets out of the scheduler were given a date", n)
	}
}

// The answer is read from the statement's own conditions rather than from one
// of them. It refuses an asset that has left the scheduler and one outside the
// perimeter alike, and answering on the lifecycle alone told a caller to wait
// for a render nothing would ever select.
func TestAManualRequestOnAnOutOfScopeAssetClaimsNothing(t *testing.T) {
	h := newHarness(t)
	id := h.renderable(t, "app.target.test:443/tcp", time.Now().Add(time.Hour), lifecycle.PriorityBaseline)
	h.exec(t, `UPDATE asset_current SET scope_status = 'out_of_scope', next_fingerprint_at = NULL
	           WHERE asset_id = $1`, id)

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", jobs, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the request answered %s", resp.Status)
	}
	if body["queued"] != false {
		t.Fatalf("an asset outside the perimeter was reported as queued: %v", body)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE asset_id = $1 AND next_fingerprint_at IS NOT NULL`,
		id); n != 0 {
		t.Fatal("an asset outside the perimeter was given a render date")
	}
}

// A browser will not open some ports at all, so promoting one puts an asset at
// the head of a queue that can never serve it.
func TestAManualRequestOnAPortABrowserRefusesClaimsNothing(t *testing.T) {
	h := newHarness(t)
	id := h.renderable(t, "app.target.test:6666/tcp", time.Now().Add(20*24*time.Hour), lifecycle.PriorityBaseline)
	h.exec(t, `UPDATE asset_current SET port = 6666 WHERE asset_id = $1`, id)

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/assets/"+id.String()+"/render", jobs, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the request answered %s", resp.Status)
	}
	if body["queued"] != false {
		t.Fatalf("a port no browser opens was reported as queued: %v", body)
	}

	due, priority := h.render(t, id)
	if !due.After(time.Now().Add(19*24*time.Hour)) || priority != lifecycle.PriorityBaseline {
		t.Fatalf("the asset was promoted anyway: due %s at priority %d", due, priority)
	}
}
