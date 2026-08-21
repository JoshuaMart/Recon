//go:build integration

// Milestone 2 over HTTP: what a run is handed, what a console is refused, and
// what separates the two.
package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
)

// token issues a console credential holding exactly these actions.
func (h *harness) token(t *testing.T, org uuid.UUID, actions ...auth.Action) string {
	t.Helper()

	secret := "console-" + uuid.NewString()
	sum := sha256.Sum256([]byte(secret))
	scopes := make([]string, 0, len(actions))
	for _, action := range actions {
		scopes = append(scopes, string(action))
	}

	h.exec(t, `INSERT INTO api_token (id, org_id, name, token_hash, scopes)
	           VALUES ($1, $2, 'console', $3, $4)`,
		uuid.New(), org, sum[:], scopes)
	return secret
}

func (h *harness) call(t *testing.T, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		payload = encoded
	}

	req, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

// due writes a host already past its resolve date.
func (h *harness) due(t *testing.T, name string) {
	t.Helper()

	id := uuid.New()
	h.exec(t, `INSERT INTO asset
		(id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1,$2,$3,'fqdn',$4,$4,'fixture','in_scope', now() - interval '1 hour', now() - interval '1 hour')`,
		id, h.org, h.program, name)
	h.exec(t, `INSERT INTO asset_current
		(asset_id, org_id, program_id, kind, key, scope_status, host,
		 next_resolve_at, next_full_at, first_seen, last_seen)
		VALUES ($1,$2,$3,'fqdn',$4,'in_scope',$4,
		        now() - interval '1 hour', now() - interval '1 hour',
		        now() - interval '1 hour', now() - interval '1 hour')`,
		id, h.org, h.program, name)
}

// A run is given its list and nothing else, and the run comes from the
// signature rather than from the path: reading it from the path first would let
// anyone probe an organization's run states by the shape of the answer.
func TestATargetListIsServedOnlyToItsOwnRun(t *testing.T) {
	h := newHarness(t)
	h.due(t, "one.target.test")

	console := h.token(t, h.org, auth.ActionManageJobs)
	resp, body := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", console,
		map[string]string{"kind": "verification", "scope": "resolve"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("starting a run answered %s: %v", resp.Status, body)
	}

	env, _ := body["env"].(map[string]any)
	targets, _ := env["FASTRECON_TARGETS_URL"].(string)
	if targets == "" {
		t.Fatalf("no target list in %v", env)
	}
	path := strings.TrimPrefix(targets, "https://recon.example")

	got, err := http.Get(h.server.URL + path)
	if err != nil {
		t.Fatalf("fetch targets: %v", err)
	}
	defer func() { _ = got.Body.Close() }()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("the run's own list answered %s", got.Status)
	}

	list := make([]byte, 1024)
	n, _ := got.Body.Read(list)
	if strings.TrimSpace(string(list[:n])) != "one.target.test" {
		t.Fatalf("the list reads %q", string(list[:n]))
	}

	// Reaching for the list is what says a scanner opened this run rather than
	// a provisioner having promised to, and those two call for opposite
	// actions when somebody finds a run sitting there.
	runID, _ := body["run_id"].(string)
	var state string
	var started *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, started_at FROM run WHERE id = $1`, runID).Scan(&state, &started); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if state != "running" || started == nil {
		t.Fatalf("after its list was fetched the run is %q started_at=%v", state, started)
	}

	// The signature names one run. Pointing it at another is a refusal rather
	// than an answer about whether that one exists.
	other, _ := h.run(t, "verification")
	swapped := strings.Replace(path, runID, other.String(), 1)
	elsewhere, err := http.Get(h.server.URL + swapped)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = elsewhere.Body.Close() }()
	if elsewhere.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a token for another run answered %s", elsewhere.Status)
	}

	// And a run in a terminal state hands out nothing further. That is the
	// effective revocation: a signed token cannot be recalled, so it stays
	// valid and stops being useful.
	h.exec(t, `UPDATE run SET state = 'completed' WHERE id = $1`, runID)
	closed, err := http.Get(h.server.URL + path)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = closed.Body.Close() }()
	if closed.StatusCode != http.StatusConflict {
		t.Fatalf("a completed run still served its list: %s", closed.Status)
	}
}

// Two situations here call for opposite actions, so the message has to name the
// run, its state and whether anything ever opened it.
func TestASecondRunIsRefusedByNamingTheFirst(t *testing.T) {
	h := newHarness(t)
	h.due(t, "one.target.test")
	console := h.token(t, h.org, auth.ActionManageJobs)

	resp, first := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", console,
		map[string]string{"scope": "resolve"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the first run answered %s: %v", resp.Status, first)
	}

	again, body := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", console,
		map[string]string{"scope": "resolve"})
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("a second run answered %s", again.Status)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, first["run_id"].(string)) {
		t.Fatalf("the refusal does not name the run: %q", detail)
	}
	if !strings.Contains(detail, "pending") {
		t.Fatalf("the refusal does not carry the run's state: %q", detail)
	}
}

// Nothing due is not a failure. A tick that finds nothing to do is the normal
// state of a healthy inventory, and answering with an error would make a
// console show one every few minutes.
func TestAnEmptyQueueIsNotAnError(t *testing.T) {
	h := newHarness(t)
	console := h.token(t, h.org, auth.ActionManageJobs)

	resp, body := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", console,
		map[string]string{"scope": "resolve"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an empty queue answered %s: %v", resp.Status, body)
	}
	if body["started"] != false {
		t.Fatalf("the answer reads %v", body)
	}
}

// Every route goes through one layer that produces a principal. Starting a run
// and entering an asset are different privileges: a credential that could do
// the first must not be able to widen the perimeter it spends its budget on.
func TestTheActionsAreSeparate(t *testing.T) {
	h := newHarness(t)
	h.due(t, "one.target.test")

	jobs := h.token(t, h.org, auth.ActionManageJobs)
	scopes := h.token(t, h.org, auth.ActionManageScope)

	refused, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/assets", jobs,
		map[string]any{"entries": []string{"typed.target.test"}})
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("a run-starting credential entered an asset: %s", refused.Status)
	}

	blocked, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", scopes,
		map[string]string{"scope": "resolve"})
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("a scope credential started a run: %s", blocked.Status)
	}

	none, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/assets", "",
		map[string]any{"entries": []string{"typed.target.test"}})
	if none.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential answered %s", none.Status)
	}

	accepted, body := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/assets", scopes,
		map[string]any{"entries": []string{"typed.target.test", "https://typed.target.test/admin"}})
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("entering an asset answered %s: %v", accepted.Status, body)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND discovery_source = 'manual'`,
		h.program); n != 3 {
		t.Fatalf("%d assets were entered, and a URL brings its host and its service with it", n)
	}
}

// A caller learning that an identifier exists in another organization is a
// cross-tenant leak of exactly one bit, and one bit is enough to enumerate.
func TestAProgrammeOfAnotherOrganizationDoesNotExist(t *testing.T) {
	h := newHarness(t)

	stranger := uuid.New()
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'other')`, stranger)
	token := h.token(t, stranger, auth.ActionManageJobs, auth.ActionManageScope)

	resp, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", token,
		map[string]string{"scope": "resolve"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another organization's programme answered %s", resp.Status)
	}

	entered, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/assets", token,
		map[string]any{"entries": []string{"typed.target.test"}})
	if entered.StatusCode != http.StatusNotFound {
		t.Fatalf("another organization's programme accepted an asset: %s", entered.Status)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 0 {
		t.Fatalf("%d assets were written into another organization's programme", n)
	}
}

// A revoked credential is refused, and so is an expired one. Both are checked
// in the statement rather than after it: a caller that forgot one would hold a
// credential nobody can take away.
func TestARevokedCredentialIsRefused(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageJobs)

	h.exec(t, `UPDATE api_token SET revoked_at = now() WHERE org_id = $1`, h.org)
	resp, _ := h.call(t, http.MethodPost, "/programs/"+h.program.String()+"/runs", token,
		map[string]string{"scope": "resolve"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked credential answered %s", resp.Status)
	}
}
