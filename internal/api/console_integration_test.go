//go:build integration

// The milestone 7 assertions that live on the contract rather than on a screen.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
)

// raw drives one console route and hands back the body untouched.
//
// Distinct from call, which decodes into a map: half the assertions below are
// about a field being absent, and a map cannot tell an absent key from one
// decoded into a zero value.
func (h *harness) raw(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, payload
}

func (h *harness) console(t *testing.T) string {
	t.Helper()

	return h.token(t, h.org, auth.ActionReadAssets, auth.ActionManageScope, auth.ActionManageJobs)
}

func decode[T any](t *testing.T, payload []byte) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return out
}

// A rule write reclassifies in the same transaction, and the due dates move
// with the verdict. Two transactions leave a window where the system scans what
// was just taken away from it.
func TestAScopeWriteMovesTheDueDatesWithIt(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	// An asset in scope with a schedule, and one outside the perimeter with
	// none, which is what the include below is going to move.
	inside, outside := uuid.New(), uuid.New()
	h.asset(t, inside, "www.target.test", "in_scope", true)
	h.asset(t, outside, "www.other.test", "unknown", false)

	resp, payload := h.raw(t, http.MethodPost,
		"/programs/"+h.program.String()+"/rules", token, map[string]any{
			"kind": "include", "matcher": "apex", "pattern": "other.test",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}

	body := decode[struct {
		Rule   map[string]any `json:"rule"`
		Effect struct {
			Examined int `json:"examined"`
			Changed  int `json:"changed"`
			Gained   int `json:"gained"`
		} `json:"effect"`
	}](t, payload)

	// The effect is returned so somebody does not have to run a search to find
	// out what they just did.
	if body.Effect.Examined < 2 {
		t.Errorf("examined = %d, want every asset of the programme", body.Effect.Examined)
	}
	if body.Effect.Changed != 1 || body.Effect.Gained != 1 {
		t.Errorf("effect = %+v, want the one asset the new include reaches", body.Effect)
	}
	if in, ok := body.Rule["in_force"].(bool); !ok || !in {
		t.Error("the rule came back out of force, so the screen would show a perimeter that is not applied")
	}

	// The half that matters: the schedule moved with the status, in the same
	// commit. A status without a due date costs coverage.
	var status string
	var due *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT scope_status, next_resolve_at FROM asset_current WHERE asset_id = $1`, outside).
		Scan(&status, &due); err != nil {
		t.Fatalf("read the reclassified asset: %v", err)
	}
	if status != "in_scope" {
		t.Errorf("scope_status = %s, want in_scope", status)
	}
	if due == nil {
		t.Error("the asset came into scope with no due date, so nothing will ever probe it")
	}

	// And asset was moved as well as its projection. Updating one would leave
	// the next report reinstating the old verdict.
	var identity string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT scope_status FROM asset WHERE id = $1`, outside).Scan(&identity); err != nil {
		t.Fatalf("read the asset identity: %v", err)
	}
	if identity != "in_scope" {
		t.Errorf("asset.scope_status = %s, want it in step with the projection", identity)
	}

	// Now the other direction. An exclude takes an asset out, and the due dates
	// have to go with it: one that keeps them goes on being scanned outside the
	// authorization.
	resp, payload = h.raw(t, http.MethodPost,
		"/programs/"+h.program.String()+"/rules", token, map[string]any{
			"kind": "exclude", "matcher": "fqdn", "pattern": "www.target.test",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}

	var excluded string
	var resolve, full, fingerprint *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT scope_status, next_resolve_at, next_full_at, next_fingerprint_at
		   FROM asset_current WHERE asset_id = $1`, inside).
		Scan(&excluded, &resolve, &full, &fingerprint); err != nil {
		t.Fatalf("read the excluded asset: %v", err)
	}
	if excluded != "out_of_scope" {
		t.Errorf("scope_status = %s, want out_of_scope", excluded)
	}
	if resolve != nil || full != nil || fingerprint != nil {
		t.Errorf("an excluded asset kept a schedule (%v, %v, %v), which is a scan outside the authorization",
			resolve, full, fingerprint)
	}
}

// A rule the perimeter cannot compile is refused, and it writes nothing. A rule
// sitting in the table that no reclassification can read makes every later
// write fail, and the one that broke it is long gone by then.
func TestAFailedRuleWritesNothing(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	before := h.count(t, `SELECT count(*) FROM scope_rule WHERE program_id = $1`, h.program)

	resp, payload := h.raw(t, http.MethodPost,
		"/programs/"+h.program.String()+"/rules", token, map[string]any{
			"kind": "include", "matcher": "regex", "pattern": "([a-z",
		})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, payload)
	}
	if after := h.count(t, `SELECT count(*) FROM scope_rule WHERE program_id = $1`, h.program); after != before {
		t.Errorf("scope_rule rows went from %d to %d on a refusal", before, after)
	}

	// The same for a matcher nothing knows. It is named rather than stored and
	// discovered later by a classification that quietly matches nothing.
	resp, _ = h.raw(t, http.MethodPost,
		"/programs/"+h.program.String()+"/rules", token, map[string]any{
			"kind": "include", "matcher": "guess", "pattern": "target.test",
		})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d on an unknown matcher, want 400", resp.StatusCode)
	}
}

// A write carrying a stale version answers 409 and applies nothing, the
// reclassification included.
func TestAStaleVersionAppliesNothing(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	resp, payload := h.raw(t, http.MethodGet, "/programs/"+h.program.String(), token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}
	current := decode[struct {
		Program struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"program"`
	}](t, payload).Program

	// An edit with the version that was read lands.
	resp, payload = h.raw(t, http.MethodPatch, "/programs/"+h.program.String(), token, map[string]any{
		"name": "renamed", "version": current.Version, "rate_limit_rps": 5,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}

	// The same version again is a caller that based a decision on a state that
	// no longer exists. It is a refusal and not a syntax error, so it has to
	// say which.
	resp, payload = h.raw(t, http.MethodPatch, "/programs/"+h.program.String(), token, map[string]any{
		"name": "renamed twice", "version": current.Version, "rate_limit_rps": 9,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, payload)
	}

	var name string
	var rate int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT name, rate_limit_rps FROM program WHERE id = $1`, h.program).Scan(&name, &rate); err != nil {
		t.Fatalf("read the programme: %v", err)
	}
	if name != "renamed" || rate != 5 {
		t.Errorf("the programme reads %q at %d rps, so the refused write applied part of itself", name, rate)
	}

	// And an edit with no version at all is refused before it reaches the
	// database. Defaulting to the current one would make the whole column
	// decorative.
	resp, _ = h.raw(t, http.MethodPatch, "/programs/"+h.program.String(), token, map[string]any{
		"name": "no version",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d on a versionless edit, want 400", resp.StatusCode)
	}
}

// A programme of another organization is a 404 and never a 403. One bit is
// enough to enumerate.
func TestAnotherTenantsProgrammeIsNotFound(t *testing.T) {
	h := newHarness(t)

	other := uuid.New()
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'other')`, other)
	token := h.token(t, other, auth.ActionReadAssets, auth.ActionManageScope)

	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/programs/" + h.program.String(), nil},
		{
			http.MethodPatch, "/programs/" + h.program.String(),
			map[string]any{"name": "taken", "version": 1},
		},
		{
			http.MethodPost, "/programs/" + h.program.String() + "/rules",
			map[string]any{"kind": "include", "matcher": "apex", "pattern": "target.test"},
		},
	} {
		resp, payload := h.call(t, probe.method, probe.path, token, probe.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404: %s", probe.method, probe.path,
				resp.StatusCode, payload)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM program WHERE org_id = $1`, other); n != 0 {
		t.Errorf("the other organization ended up with %d programmes", n)
	}
}

// The counters cost a scan per programme, so they are asked for rather than
// given: the switcher renders this list on every page.
func TestTheProgrammeCountersAreAskedFor(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)
	h.asset(t, uuid.New(), "www.target.test", "in_scope", true)

	_, payload := h.raw(t, http.MethodGet, "/programs", token, nil)
	plain := decode[struct {
		Programs []map[string]any `json:"programs"`
	}](t, payload).Programs
	if len(plain) != 1 {
		t.Fatalf("programmes = %d, want 1", len(plain))
	}
	if _, present := plain[0]["assets"]; present {
		t.Error("the default list carries the counters, and it is the shape the switcher renders")
	}
	if _, present := plain[0]["rules_in_force"]; !present {
		t.Error("the list carries no rule count, which costs nothing and is what the screen reads")
	}

	_, payload = h.raw(t, http.MethodGet, "/programs?counts=1", token, nil)
	counted := decode[struct {
		Programs []struct {
			Assets        *int `json:"assets"`
			AssetsInScope *int `json:"assets_in_scope"`
		} `json:"programs"`
	}](t, payload).Programs
	if len(counted) != 1 || counted[0].Assets == nil || *counted[0].Assets != 1 {
		t.Fatalf("counted = %+v, want one asset", counted)
	}
	if counted[0].AssetsInScope == nil || *counted[0].AssetsInScope != 1 {
		t.Errorf("in scope = %v, want 1", counted[0].AssetsInScope)
	}
}

// The queue answers "why is nothing moving", and the three numbers are
// disjoint: a row held by a run is not also due.
func TestTheQueueSeparatesDueFromHeld(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	due, held, later, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	h.asset(t, due, "due.target.test", "in_scope", true)
	h.asset(t, held, "held.target.test", "in_scope", true)
	h.asset(t, later, "later.target.test", "in_scope", true)
	h.asset(t, gone, "gone.target.test", "in_scope", false)

	h.exec(t, `UPDATE asset_current SET next_resolve_at = now() + interval '1 day' WHERE asset_id = $1`, later)

	runID := uuid.New()
	h.exec(t, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
	           VALUES ($1, $2, $3, 'verification', 'full', 'running', now() + interval '1 hour')`,
		runID, h.org, h.program)
	h.exec(t, `INSERT INTO run_target (run_id, asset_id, org_id, key)
	           VALUES ($1, $2, $3, 'held.target.test')`, runID, held, h.org)

	_, payload := h.raw(t, http.MethodGet, "/queue", token, nil)
	body := decode[struct {
		Depths []struct {
			Queue string `json:"queue"`
			Due   int    `json:"due"`
			Later int    `json:"later"`
			InRun int    `json:"in_run"`
		} `json:"depths"`
		Runs []struct {
			ID    uuid.UUID `json:"id"`
			State string    `json:"state"`
		} `json:"runs"`
	}](t, payload)

	var resolve *struct {
		Queue string `json:"queue"`
		Due   int    `json:"due"`
		Later int    `json:"later"`
		InRun int    `json:"in_run"`
	}
	for i := range body.Depths {
		if body.Depths[i].Queue == "resolve" {
			resolve = &body.Depths[i]
		}
	}
	if resolve == nil {
		t.Fatalf("no resolve queue in %s", payload)
	}
	if resolve.Due != 1 {
		t.Errorf("due = %d, want the one asset nothing holds", resolve.Due)
	}
	if resolve.InRun != 1 {
		t.Errorf("in_run = %d, want the one a live run holds", resolve.InRun)
	}
	if resolve.Later != 1 {
		t.Errorf("later = %d, want the one scheduled ahead", resolve.Later)
	}
	// The asset with no due date is counted nowhere. Filing it under later
	// would show a queue that never drains.
	if total := resolve.Due + resolve.Later + resolve.InRun; total != 3 {
		t.Errorf("the three numbers add to %d over four assets, and the fourth has left the scheduler", total)
	}

	if len(body.Runs) != 1 || body.Runs[0].ID != runID {
		t.Errorf("runs = %+v, want the one in flight: without them an empty queue "+
			"and a full one look alike", body.Runs)
	}
}

// asset writes a projected asset the way ingestion would, which is what these
// tests need and what a report would take three round trips to produce.
func (h *harness) asset(t *testing.T, id uuid.UUID, key, status string, scheduled bool) {
	t.Helper()

	h.exec(t, `INSERT INTO asset (id, org_id, program_id, kind, key, host,
	                              discovery_source, scope_status, first_seen, last_seen)
	           VALUES ($1, $2, $3, 'fqdn', $4, $4, 'test', $5, now(), now())`,
		id, h.org, h.program, key, status)

	// All three due dates, not just one. A fixture that leaves two of them null
	// lets an exclusion that clears only the first pass the assertion below:
	// the other two read null either way, and two thirds of the guard proves
	// nothing. Found by removing one branch and watching the test stay green.
	due := "NULL, NULL, NULL"
	if scheduled {
		due = "now(), now(), now()"
	}
	h.exec(t, fmt.Sprintf(`
		INSERT INTO asset_current (asset_id, org_id, program_id, kind, key, host,
		                           scope_status, lifecycle, first_seen, last_seen,
		                           next_resolve_at, next_full_at, next_fingerprint_at)
		VALUES ($1, $2, $3, 'fqdn', $4, $4, $5, 'active', now(), now(), %s)`, due),
		id, h.org, h.program, key, status)
}

// A service discovered and never probed is found by a filter on the port its key
// carries.
//
// The port is promoted at creation rather than waited for from an observation,
// which is what makes this true: a service that nothing has answered on is
// exactly the interesting case, and a filter that only worked after a probe
// would hide the assets somebody came looking for.
func TestAnUnprobedServiceIsFoundByItsPort(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	resp, payload := h.raw(t, http.MethodPost,
		"/programs/"+h.program.String()+"/assets", token, map[string]any{
			"entries": []string{"https://app.target.test", "plain.target.test"},
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}

	resp, payload = h.raw(t, http.MethodPost, "/assets/search", token, map[string]any{
		"filter": map[string]any{"op": "eq", "field": "port", "value": 443},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, payload)
	}

	found := decode[struct {
		Assets []struct {
			Key           string  `json:"key"`
			Port          *int    `json:"port"`
			LastCheckedAt *string `json:"last_checked_at"`
		} `json:"assets"`
	}](t, payload).Assets

	if len(found) == 0 {
		t.Fatalf("a filter on port 443 matched nothing: %s", payload)
	}
	for _, asset := range found {
		if asset.Port == nil || *asset.Port != 443 {
			t.Errorf("%s came back on a port filter with port %v", asset.Key, asset.Port)
		}
		// The half that makes the assertion mean something: none of these has
		// ever been probed, so the column cannot have come from an observation.
		if asset.LastCheckedAt != nil {
			t.Errorf("%s has been probed, so this proves nothing about promotion at creation", asset.Key)
		}
	}
}

// The enrichment state is served rather than deduced, because the console cannot
// tell "no database configured" from "configured with no match" by looking at the
// data: no asset carries an ASN in either case.
func TestTheDeploymentSaysWhetherItEnriches(t *testing.T) {
	h := newHarness(t)

	_, payload := h.raw(t, http.MethodGet, "/assets/fields", h.console(t), nil)
	body := decode[struct {
		Fields     map[string][]string `json:"fields"`
		Enrichment *struct {
			Configured bool `json:"configured"`
		} `json:"enrichment"`
	}](t, payload)

	if body.Enrichment == nil {
		t.Fatal("the vocabulary says nothing about enrichment, so the console would have to guess")
	}
	if body.Enrichment.Configured {
		t.Error("this harness has no MaxMind database and the endpoint claims it enriches")
	}
	// And the vocabulary itself, which is the other half of the same idea: a
	// console learning what it may filter on against 400s learns it wrong.
	if len(body.Fields["program_id"]) == 0 {
		t.Error("program_id is not in the vocabulary, so the switcher cannot filter on it")
	}
	if _, present := body.Fields["org_id"]; present {
		t.Error("org_id is in the vocabulary: a tenant filter a caller can express is one " +
			"a caller can forget")
	}
}

// The queue carries what the platform called each execution.
//
// Without it the logs of a run that went wrong are unfindable, which is the
// whole of that column's purpose. It has to come back on the read rather than
// only on the answer that started the run: the answer lives for one request,
// and somebody looking for an execution's logs is looking after a reload.
func TestTheQueueCarriesTheExecutionIdentifier(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)

	started, unstarted := uuid.New(), uuid.New()
	h.exec(t, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, external_id)
	           VALUES ($1, $2, $3, 'discovery', 'full', 'pending', now() + interval '1 hour', 'job-exec-1')`,
		started, h.org, h.program)
	h.exec(t, `INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline)
	           VALUES ($1, $2, $3, 'verification', 'full', 'pending', now() + interval '1 hour')`,
		unstarted, h.org, h.program)

	_, payload := h.raw(t, http.MethodGet, "/queue", token, nil)
	runs := decode[struct {
		Runs []struct {
			ID         uuid.UUID `json:"id"`
			ExternalID *string   `json:"external_id"`
		} `json:"runs"`
	}](t, payload).Runs

	found := map[uuid.UUID]*string{}
	for _, run := range runs {
		found[run.ID] = run.ExternalID
	}
	if got := found[started]; got == nil || *got != "job-exec-1" {
		t.Errorf("the started run came back with external_id %v, want the platform's identifier", got)
	}
	// And absent stays absent. A run nothing started and one whose execution is
	// unnamed would otherwise read the same, and they want opposite actions.
	if got, present := found[unstarted]; !present || got != nil {
		t.Errorf("the unstarted run came back with external_id %v, want none", got)
	}
}
