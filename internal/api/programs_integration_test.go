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
