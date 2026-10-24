package scope

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "k" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/", "k", 5*time.Second)
}

func TestClientDecodes(t *testing.T) {
	c := serve(t, 200, `{"scopes":[{"slug":"acme","platform":"hackerone","program_name":"Acme","bounty":true,"value":"*.acme.com","type":"web","is_in_scope":true}],"count":1}`)
	got, err := c.Scopes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{Slug: "acme", Platform: "hackerone", ProgramName: "Acme", Bounty: true, Value: "*.acme.com", Type: "web", IsInScope: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v", got)
	}
}

func TestClientRefusesWhatCannotBeTrusted(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"count mismatch": {200, `{"scopes":[],"count":3}`, "does not match"},
		"no count":       {200, `{"scopes":[]}`, "does not match"},
		"server error":   {500, `boom`, "500"},
		"not json":       {200, `<html>`, "decode"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := serve(t, tc.status, tc.body).Scopes(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientSendsTheKey(t *testing.T) {
	c := serve(t, 200, `{"scopes":[],"count":0}`)
	c.key = "wrong"
	if _, err := c.Scopes(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}
