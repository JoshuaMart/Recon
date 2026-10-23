//go:build integration

package api

import (
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
