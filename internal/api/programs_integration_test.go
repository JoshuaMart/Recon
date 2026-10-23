//go:build integration

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoshuaMart/recon/internal/store/storetest"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	st := storetest.New(t)
	auth, err := NewAuth(testKey, "http://recon.test")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(Deps{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: st, Store: st, Auth: auth, DefaultAutoScan: "off",
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t, srv}
}

func (c *client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestManualProgramLifecycle(t *testing.T) {
	c := newClient(t)

	code, p := c.do("POST", "/api/programs", `{"name":"MSRC","platform":"Microsoft"}`)
	if code != 201 || p["origin"] != "manual" || p["platform"] != "microsoft" || p["effective_auto_scan"] != "off" {
		t.Fatalf("create = %d %v", code, p)
	}
	id := p["id"].(string)

	code, rule := c.do("POST", "/api/programs/"+id+"/rules", `{"kind":"include","matcher":"apex","pattern":"*.Microsoft.com"}`)
	if code != 201 || rule["pattern"] != "microsoft.com" || rule["origin"] != "local" {
		t.Fatalf("open rule = %d %v", code, rule)
	}
	if code, _ := c.do("POST", "/api/programs/"+id+"/rules", `{"kind":"include","matcher":"apex","pattern":"microsoft.com"}`); code != 409 {
		t.Fatalf("same rule twice = %d, want 409", code)
	}
	if code, body := c.do("POST", "/api/programs/"+id+"/rules", `{"kind":"include","matcher":"regex","pattern":"x"}`); code != 400 || body["error"] != "bad_rule" {
		t.Fatalf("bad matcher = %d %v", code, body)
	}

	code, wl := c.do("GET", "/api/wildcards", "")
	if ws := wl["wildcards"].([]any); code != 200 || len(ws) != 1 || ws[0].(map[string]any)["apex"] != "microsoft.com" {
		t.Fatalf("wildcards = %d %v", code, wl)
	}

	// Editing closes and reopens: the history keeps both.
	ruleID := rule["id"].(string)
	code, edited := c.do("PATCH", "/api/programs/"+id+"/rules/"+ruleID, `{"pattern":"azure.com"}`)
	if code != 200 || edited["pattern"] != "azure.com" || edited["id"] == ruleID {
		t.Fatalf("edit = %d %v", code, edited)
	}
	if code, _ := c.do("PATCH", "/api/programs/"+id+"/rules/"+ruleID, `{"close":true}`); code != 409 {
		t.Fatalf("closing a closed rule = %d, want 409", code)
	}
	_, full := c.do("GET", "/api/programs/"+id, "")
	rules := full["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["in_force"] != false || rules[1].(map[string]any)["in_force"] != true {
		t.Fatalf("rules after edit = %v", rules)
	}
	if ws := full["wildcards"].([]any); len(ws) != 2 {
		t.Fatalf("both wildcards are kept, one inactive: %v", ws)
	}

	// Archive by hand, then the auto-scan override, then back to inherited.
	code, p = c.do("PATCH", "/api/programs/"+id, `{"state":"archived","auto_scan":"on_new"}`)
	if code != 200 || p["state"] != "archived" || p["archived_by"] != "user" || p["effective_auto_scan"] != "on_new" {
		t.Fatalf("patch = %d %v", code, p)
	}
	code, p = c.do("PATCH", "/api/programs/"+id, `{"state":"active","auto_scan":null}`)
	if code != 200 || p["archived_by"] != nil || p["auto_scan"] != nil || p["effective_auto_scan"] != "off" {
		t.Fatalf("patch back = %d %v", code, p)
	}
	if code, _ := c.do("PATCH", "/api/programs/"+id, `{"colour":"red"}`); code != 400 {
		t.Fatalf("unknown field = %d, want 400", code)
	}
}

func TestUnknownAndMalformedIDsAnswer404(t *testing.T) {
	c := newClient(t)
	for _, path := range []string{"/api/programs/0190b7c4-0000-7000-8000-000000000000", "/api/programs/not-an-id"} {
		if code, body := c.do("GET", path, ""); code != 404 || body["error"] != "not_found" {
			t.Errorf("%s = %d %v", path, code, body)
		}
	}
	if code, _ := c.do("GET", "/api/nothing-here", ""); code != 404 {
		t.Errorf("unknown route = %d", code)
	}
	if code, body := c.do("POST", "/api/sync", ""); code != 503 || body["error"] != "sync_disabled" {
		t.Errorf("sync without ScopesExtractor = %d %v", code, body)
	}
}

func TestSearchRoutes(t *testing.T) {
	c := newClient(t)
	code, p := c.do("POST", "/api/programs", `{"name":"Acme"}`)
	if code != 201 {
		t.Fatal(code)
	}
	pid := p["id"].(string)
	if code, _ := c.do("POST", "/api/programs/"+pid+"/rules", `{"kind":"include","matcher":"apex","pattern":"acme.com"}`); code != 201 {
		t.Fatal(code)
	}
	code, body := c.do("POST", "/api/programs/"+pid+"/assets", `{"entries":["https://app.acme.com/portal"]}`)
	if code != 200 {
		t.Fatalf("declare = %d %v", code, body)
	}

	code, body = c.do("GET", "/api/search?q=techs:nginx", "")
	if code != 400 || body["error"] != "unknown_field" || body["suggest"] != "tech" {
		t.Errorf("unknown field = %d %v", code, body)
	}
	if code, body = c.do("GET", "/api/search?q=title:%22x", ""); code != 400 || body["error"] != "syntax_error" {
		t.Errorf("syntax error = %d %v", code, body)
	}
	code, body = c.do("GET", "/api/search?view=assets&q=apex:acme.com", "")
	rows, _ := body["rows"].([]any)
	if code != 200 || len(rows) != 3 || body["query"] != "apex:acme.com" {
		t.Fatalf("search = %d %v", code, body)
	}
	if code, body = c.do("GET", "/api/search/fields", ""); code != 200 || len(body["fields"].([]any)) < 30 {
		t.Errorf("fields = %d", code)
	}
	if code, body = c.do("GET", "/api/search/facets?view=assets", ""); code != 200 || body["facets"] == nil {
		t.Errorf("facets = %d %v", code, body)
	}

	// The asset view and triage, on the declared URL.
	var urlID string
	for _, r := range rows {
		row := r.(map[string]any)
		if row["kind"] == "url" {
			urlID = row["asset_id"].(string)
		}
	}
	code, body = c.do("GET", "/api/assets/"+urlID, "")
	if code != 200 || body["asset"].(map[string]any)["discovery_source"] != "manual" || body["triage"].(map[string]any)["state"] != "new" {
		t.Fatalf("asset view = %d %v", code, body)
	}
	cur := body["current"].(map[string]any)
	if cur["lifecycle"] != "candidate" || cur["next_render_at"] != nil {
		t.Errorf("a declared URL whose service never answered: %v / render at %v", cur["lifecycle"], cur["next_render_at"])
	}
	code, body = c.do("PATCH", "/api/assets/"+urlID+"/triage", `{"state":"interesting","tags":["IDOR"," idor ","todo"],"seen":true}`)
	if code != 200 || body["state"] != "interesting" || len(body["tags"].([]any)) != 2 || body["seen_at"] == nil {
		t.Fatalf("triage = %d %v", code, body)
	}
	_, body = c.do("GET", "/api/search?view=assets&q=triage:interesting%20tag:idor", "")
	if rows := body["rows"].([]any); len(rows) != 1 {
		t.Errorf("triage filter = %v", body)
	}
	if code, _ := c.do("PATCH", "/api/assets/"+urlID+"/triage", `{"state":"loved"}`); code != 400 {
		t.Errorf("bad triage state = %d", code)
	}
}

func TestExportFormats(t *testing.T) {
	c := newClient(t)
	_, p := c.do("POST", "/api/programs", `{"name":"Acme"}`)
	pid := p["id"].(string)
	c.do("POST", "/api/programs/"+pid+"/rules", `{"kind":"include","matcher":"apex","pattern":"acme.com"}`)
	c.do("POST", "/api/programs/"+pid+"/assets", `{"entries":["https://app.acme.com/portal","api.acme.com"]}`)
	for format, want := range map[string]string{
		"urls":  "https://app.acme.com/portal\n",
		"hosts": "app.acme.com\n",
	} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", c.srv.URL+"/api/search/export?format="+format+"&q=kind:url", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || string(b) != want {
			t.Errorf("export %s = %d %q, want %q", format, res.StatusCode, b, want)
		}
	}
	// A query that parses and does not compile is refused before the body starts.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", c.srv.URL+"/api/search/export?format=urls&q=techs:nginx", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("export with an unknown field = %d, want 400", res.StatusCode)
	}
	req, _ = http.NewRequestWithContext(t.Context(), "GET", c.srv.URL+"/api/search/export?format=xml", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("unknown format = %d", res.StatusCode)
	}
}

// Two triage edits in flight both land: one marks the asset seen, the other
// tags it.
func TestConcurrentTriageEditsBothLand(t *testing.T) {
	c := newClient(t)
	_, p := c.do("POST", "/api/programs", `{"name":"Acme"}`)
	pid := p["id"].(string)
	c.do("POST", "/api/programs/"+pid+"/rules", `{"kind":"include","matcher":"apex","pattern":"acme.com"}`)
	c.do("POST", "/api/programs/"+pid+"/assets", `{"entries":["api.acme.com"]}`)
	_, page := c.do("GET", "/api/search?view=assets&q=host:api.acme.com", "")
	id := page["rows"].([]any)[0].(map[string]any)["asset_id"].(string)
	for range 10 {
		done := make(chan struct{}, 2)
		go func() { c.do("PATCH", "/api/assets/"+id+"/triage", `{"tags":["todo"]}`); done <- struct{}{} }()
		go func() { c.do("PATCH", "/api/assets/"+id+"/triage", `{"note":"look at /admin"}`); done <- struct{}{} }()
		<-done
		<-done
		_, a := c.do("GET", "/api/assets/"+id, "")
		tr := a["triage"].(map[string]any)
		if tags, _ := tr["tags"].([]any); len(tags) != 1 || tr["note"] != "look at /admin" {
			t.Fatalf("one edit erased the other: %v", tr)
		}
		c.do("PATCH", "/api/assets/"+id+"/triage", `{"tags":[],"note":""}`)
	}
}
