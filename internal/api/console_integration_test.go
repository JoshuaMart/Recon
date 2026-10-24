//go:build integration

package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

func assetID(t *testing.T, c *client, program, key string) string {
	t.Helper()
	a, err := c.st.Q.GetAssetByKey(t.Context(), sqlcgen.GetAssetByKeyParams{ProgramID: uuid.MustParse(program), Kind: "fqdn", Key: key})
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return a.ID.String()
}

func TestSavedSearches(t *testing.T) {
	c := newClient(t)

	code, s := c.do("POST", "/api/saved-searches", `{"name":"Old nginx","query":"tech:nginx@<1.25"}`)
	if code != 201 || s["view"] != "services" || s["alert"] != false {
		t.Fatalf("create = %d %v", code, s)
	}
	id := s["id"].(string)
	if code, body := c.do("POST", "/api/saved-searches", `{"name":"Old nginx","query":"port:443"}`); code != 409 {
		t.Fatalf("same name = %d %v, want 409", code, body)
	}
	// A query the language refuses would fail every time it is opened.
	if code, body := c.do("POST", "/api/saved-searches", `{"name":"Typo","query":"techh:nginx"}`); code != 400 {
		t.Fatalf("bad query = %d %v, want 400", code, body)
	}
	if code, body := c.do("PATCH", "/api/saved-searches/"+id, `{"view":"assets"}`); code != 400 {
		t.Fatalf("bad view = %d %v, want 400", code, body)
	}
	code, s = c.do("PATCH", "/api/saved-searches/"+id, `{"alert":true}`)
	if code != 200 || s["alert"] != true || s["query"] != "tech:nginx@<1.25" {
		t.Fatalf("patch = %d %v, want the query kept", code, s)
	}
	if code, l := c.do("GET", "/api/saved-searches", ""); code != 200 || len(l["saved_searches"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, l)
	}
	if code, _ := c.do("DELETE", "/api/saved-searches/"+id, ""); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := c.do("DELETE", "/api/saved-searches/"+id, ""); code != 404 {
		t.Fatalf("delete twice = %d, want 404", code)
	}
}

func declare(t *testing.T, c *client, entries string) (string, []any) {
	t.Helper()
	_, p := c.do("POST", "/api/programs", `{"name":"Acme"}`)
	id := p["id"].(string)
	if code, body := c.do("POST", "/api/programs/"+id+"/rules", `{"kind":"include","matcher":"apex","pattern":"acme.com"}`); code != 201 {
		t.Fatalf("rule = %d %v", code, body)
	}
	code, d := c.do("POST", "/api/programs/"+id+"/assets", `{"entries":`+entries+`}`)
	if code != 200 {
		t.Fatalf("declare = %d %v", code, d)
	}
	return id, d["entries"].([]any)
}

func TestChangesFeed(t *testing.T) {
	c := newClient(t)
	declare(t, c, `["a.acme.com","b.acme.com","c.acme.com"]`)

	code, f := c.do("GET", "/api/changes", "")
	rows := f["changes"].([]any)
	if code != 200 || len(rows) < 3 || f["next"] != nil {
		t.Fatalf("feed = %d %v", code, f)
	}
	first := rows[0].(map[string]any)
	if first["program"] != "Acme" || first["asset_key"] == "" {
		t.Fatalf("row = %v", first)
	}
	if code, body := c.do("GET", "/api/changes?cursor=nope", ""); code != 400 {
		t.Fatalf("bad cursor = %d %v", code, body)
	}

	count := func(path string) int {
		t.Helper()
		code, f := c.do("GET", path, "")
		if code != 200 {
			t.Fatalf("%s = %d %v", path, code, f)
		}
		return len(f["changes"].([]any))
	}
	if n := count("/api/changes?q=" + url.QueryEscape("change:new_asset host:a.acme.com")); n != 1 {
		t.Fatalf("change: and the language together = %d rows, want 1", n)
	}
	if n := count("/api/changes?q=change:port_opened"); n != 0 {
		t.Fatalf("change:port_opened = %d rows, want 0", n)
	}
	_, before := c.do("GET", "/api/changes/unseen", "")

	// Marking as seen what a query shows leaves the rest unseen.
	if code, r := c.do("POST", "/api/changes/seen", `{"q":"host:a.acme.com"}`); code != 200 || r["assets"].(float64) != 1 {
		t.Fatalf("mark seen = %d %v", code, r)
	}
	if n := count("/api/changes?q=host:a.acme.com"); n != 0 {
		t.Fatalf("a seen asset's changes still unseen: %d", n)
	}
	if n := count("/api/changes?view=all&q=host:a.acme.com"); n != 1 {
		t.Fatalf("the all view lost a seen change: %d", n)
	}
	if n := count("/api/changes?q=host:b.acme.com"); n != 1 {
		t.Fatalf("b was marked seen too: %d", n)
	}
	_, after := c.do("GET", "/api/changes/unseen", "")
	if after["unseen"].(float64) != before["unseen"].(float64)-1 {
		t.Fatalf("unseen %v then %v, want one less", before["unseen"], after["unseen"])
	}

	// change: belongs to the feed: search refuses it rather than ignoring it.
	if code, body := c.do("GET", "/api/search?q=change:new_asset", ""); code != 400 || body["error"] != "unknown_field" {
		t.Fatalf("change: in search = %d %v", code, body)
	}
}

func TestProbeNow(t *testing.T) {
	c := newClientWith(t, func(d *Deps) { d.Verification = true })
	pid, _ := declare(t, c, `["a.acme.com","x.other.com"]`)
	a := assetID(t, c, pid, "a.acme.com")
	if code, r := c.do("POST", "/api/assets/"+a+"/probe", ""); code != 200 || r["queued"] != true {
		t.Fatalf("probe in scope = %d %v", code, r)
	}
	// First in line: before any host overdue for a day, not merely due now.
	cur, err := c.st.Q.GetAssetCurrent(t.Context(), uuid.MustParse(a))
	if err != nil || cur.NextFullAt == nil || !cur.NextFullAt.Before(time.Now().Add(-24*time.Hour)) {
		t.Fatalf("next_full_at = %v %v, want before any backlog", cur.NextFullAt, err)
	}
	if code, r := c.do("POST", "/api/assets/"+assetID(t, c, pid, "x.other.com")+"/probe", ""); code != 200 || r["queued"] != false || r["reason"] == "" {
		t.Fatalf("probe out of scope = %d %v", code, r)
	}

	off := newClient(t)
	pid, _ = declare(t, off, `["a.acme.com"]`)
	id := assetID(t, off, pid, "a.acme.com")
	if code, r := off.do("POST", "/api/assets/"+id+"/probe", ""); r["queued"] != false || r["reason"] != "verification is disabled" {
		t.Fatalf("probe with verification off = %d %v", code, r)
	}
}

func TestActivity(t *testing.T) {
	c := newClient(t)
	pid, _ := declare(t, c, `["a.acme.com"]`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.srv.URL+"/api/activity", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("activity without a session = %d, want 401: it says why a component fails", res.StatusCode)
	}
	if _, err := c.st.Q.EnqueueRun(t.Context(), sqlcgen.EnqueueRunParams{ProgramID: uuid.MustParse(pid), Kind: "verification", Stage: "resolve", Trigger: "schedule", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	since, detail := time.Now().Add(-time.Hour), "connection refused"
	if err := c.st.Q.PutHealthState(t.Context(), sqlcgen.PutHealthStateParams{Component: "sync", FailingSince: &since, Detail: &detail}); err != nil {
		t.Fatal(err)
	}
	if err := c.st.Q.PutHealthState(t.Context(), sqlcgen.PutHealthStateParams{Component: "render"}); err != nil {
		t.Fatal(err)
	}

	code, a := c.do("GET", "/api/activity", "")
	if code != 200 {
		t.Fatalf("activity = %d %v", code, a)
	}
	runs := a["runs"].([]any)
	if len(runs) != 1 || a["queued"] != float64(1) || runs[0].(map[string]any)["program"] != "Acme" {
		t.Fatalf("runs = %v queued = %v", runs, a["queued"])
	}
	// Only what fails now: a component that recovered is not a problem.
	problems := a["problems"].([]any)
	if len(problems) != 1 || problems[0].(map[string]any)["component"] != "sync" || problems[0].(map[string]any)["detail"] != detail {
		t.Fatalf("problems = %v", problems)
	}
}
