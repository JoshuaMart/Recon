//go:build integration

// Milestone 9 over HTTP: what an import writes, what it refuses, and who is
// allowed to perform one.
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
)

// stream posts a body as it is, which the JSON helper next door cannot do: the
// endpoint takes newline delimited JSON and a marshalled string would arrive as
// a quoted document.
func (h *harness) stream(t *testing.T, path, token, body string) (*http.Response, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

// events builds a stream naming hosts inside the harness perimeter.
func events(names ...string) string {
	lines := make([]string, 0, len(names))
	for i, name := range names {
		lines = append(lines, fmt.Sprintf(
			`{"type":"DNS_NAME","id":"DNS_NAME:%d","data":%q,"host":%q,"module":"crt_db",`+
				`"timestamp":1785346188.5,"scope_description":"in-scope",`+
				`"discovery_context":"crt_db found %s"}`, i, name, name, name))
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestAnImportCreatesInventoryAndSaysWhatItDid(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageScope)

	body := events("api.target.test", "www.target.test", "elsewhere.example.com") +
		`{"type":"OPEN_TCP_PORT","id":"P:1","host":"api.target.test","port":443,` +
		`"module":"portscan","timestamp":1785346190.0}` + "\n"

	resp, answer := h.stream(t, "/programs/"+h.program.String()+"/imports/bbot", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, answer = %v", resp.StatusCode, answer)
	}

	assets, _ := answer["assets"].(map[string]any)
	if assets == nil {
		t.Fatalf("the answer carries no accounting: %v", answer)
	}
	if created := assets["created"].(float64); created != 4 {
		t.Errorf("created = %v, want three names and one service", created)
	}
	// Two in the perimeter, one outside it, and the service inherits its host.
	byScope, _ := assets["by_scope"].(map[string]any)
	if byScope["in_scope"] != float64(3) || byScope["unknown"] != float64(1) {
		t.Errorf("by_scope = %v, want three in scope and one unknown", byScope)
	}
	// Only the two in-perimeter hosts are scheduled. A service never is.
	if scheduled := assets["scheduled"].(float64); scheduled != 2 {
		t.Errorf("scheduled = %v, want the two hosts inside the perimeter", scheduled)
	}

	// Every type in the file is accounted for, including the one that produced
	// no asset of its own.
	types, _ := answer["events"].(map[string]any)
	if len(types) != 2 {
		t.Errorf("events = %v, want both types named", types)
	}

	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND discovery_source = 'bbot'`,
		h.program); n != 4 {
		t.Errorf("%d assets carry the import as their source", n)
	}
	if n := h.count(t, `SELECT count(*) FROM observation`); n != 0 {
		t.Errorf("%d observations were written, and an import measures nothing", n)
	}
}

// The real file, into a programme that has no rule for the perimeter it
// describes. It is the wrong-paste case, and the answer is that everything is
// stored and nothing is looked at.
func TestARealScanIntoTheWrongProgrammeSchedulesNothing(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageScope)

	file, err := os.ReadFile("../bbot/testdata/scan.ndjson")
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}

	resp, answer := h.stream(t, "/programs/"+h.program.String()+"/imports/bbot", token, string(file))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, answer = %v", resp.StatusCode, answer)
	}

	assets, _ := answer["assets"].(map[string]any)
	if assets["created"].(float64) == 0 {
		t.Fatal("nothing was stored, and the point is that it is stored and not probed")
	}
	if scheduled := assets["scheduled"].(float64); scheduled != 0 {
		t.Errorf("scheduled = %v, want nothing from a perimeter with no rule", scheduled)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE program_id = $1 AND next_resolve_at IS NOT NULL`,
		h.program); n != 0 {
		t.Errorf("%d rows carry a due date and a run would take them", n)
	}
	if answer["scan"].(map[string]any)["name"] != "reference" {
		t.Errorf("the answer does not name the scan it read: %v", answer["scan"])
	}
}

// The shape jq produces. Refusing it by name is what keeps somebody from
// reading "malformed" about a body that is valid JSON.
func TestAJSONArrayIsRefusedByNameOverHTTP(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageScope)

	resp, answer := h.stream(t, "/programs/"+h.program.String()+"/imports/bbot", token,
		`[{"type":"DNS_NAME","data":"api.target.test"}]`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(answer["detail"].(string), "newline delimited") {
		t.Errorf("detail = %q, want it to name the shape it wanted", answer["detail"])
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 0 {
		t.Errorf("%d assets were written by a refused import", n)
	}
}

// One bad line costs itself. A stream from a process that was killed ends in
// half a line, and losing the other four hundred over it is the wrong trade.
func TestAMalformedLineIsNamedAndTheRestOfTheFileLands(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageScope)

	body := events("good.target.test") +
		`{"type":"DNS_NAME","id":"broken","data":"half.target.test"` + "\n" +
		events("after.target.test")

	resp, answer := h.stream(t, "/programs/"+h.program.String()+"/imports/bbot", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, answer = %v", resp.StatusCode, answer)
	}

	refused, _ := answer["refused"].([]any)
	if len(refused) != 1 {
		t.Errorf("refused = %v, want the one line that was not an event", refused)
	}
	for _, want := range []string{"good.target.test", "after.target.test"} {
		if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND key = $2`,
			h.program, want); n != 1 {
			t.Errorf("%s is missing, so a bad line cost more than itself", want)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND key = $2`,
		h.program, "half.target.test"); n != 0 {
		t.Error("the malformed line was written")
	}
}

// Refused rather than truncated. An import that silently kept the first ten
// thousand would report a smaller perimeter than the caller handed over.
func TestAnImportOverTheBoundIsRefusedRatherThanTruncated(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, h.org, auth.ActionManageScope)

	names := make([]string, 0, 10_001)
	for i := range 10_001 {
		names = append(names, fmt.Sprintf("h%d.target.test", i))
	}

	resp, answer := h.stream(t, "/programs/"+h.program.String()+"/imports/bbot", token, events(names...))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, answer = %v", resp.StatusCode, answer)
	}
	if answer["error"] != "too_many" {
		t.Errorf("error = %v, want it named", answer["error"])
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 0 {
		t.Errorf("%d assets were written by a refused import, which is the truncation this refuses", n)
	}
}

// An import asserts something about a perimeter, so it holds manage_scope. A
// credential holding everything else must not reach it.
func TestImportingNeedsTheScopeActionAndTheProgramme(t *testing.T) {
	h := newHarness(t)
	path := "/programs/" + h.program.String() + "/imports/bbot"
	body := events("api.target.test")

	if resp, _ := h.stream(t, path, "", body); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential answered %d, want 401", resp.StatusCode)
	}

	// Every action but the one this route holds, so the refusal is about the
	// action rather than about the token being weak.
	weak := h.token(t, h.org, auth.ActionReadAssets, auth.ActionManageJobs, auth.ActionIngest)
	if resp, _ := h.stream(t, path, weak, body); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token without manage_scope answered %d, want 403", resp.StatusCode)
	}

	// Another organization's programme is a 404 and not a 403: the difference
	// between the two enumerates what exists, and one bit is enough.
	other := uuid.New()
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'other')`, other)
	stranger := h.token(t, other, auth.ActionManageScope)
	if resp, _ := h.stream(t, path, stranger, body); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another organization answered %d, want 404", resp.StatusCode)
	}

	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 0 {
		t.Errorf("%d assets were written by a refused caller", n)
	}

	// And the positive control, so the three refusals above are not passing on
	// a route that refuses everyone.
	good := h.token(t, h.org, auth.ActionManageScope)
	if resp, answer := h.stream(t, path, good, body); resp.StatusCode != http.StatusOK {
		t.Errorf("the right credential answered %d: %v", resp.StatusCode, answer)
	}
}
