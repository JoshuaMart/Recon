package ui

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const page = `<!doctype html><html><head><script type="module" src="/_app/x.js"></script></head>` +
	`<body><script>boot()</script></body></html>`

func get(t *testing.T, h http.Handler, method, p string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, p, nil))
	return rec
}

func TestHandler(t *testing.T) {
	h := Handler(fstest.MapFS{
		"index.html":              {Data: []byte(page)},
		"_app/immutable/entry.js": {Data: []byte("1")},
		"favicon.svg":             {Data: []byte("<svg/>")},
	})

	for _, p := range []string{"/", "/assets/0193", "/wildcards"} {
		rec := get(t, h, "GET", p)
		if rec.Code != 200 || rec.Body.String() != page || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s = %d %q, want the page", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if rec := get(t, h, "GET", "/_app/immutable/entry.js"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("immutable chunk = %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(t, h, "GET", "/_app/immutable/gone.js"); rec.Code != 404 {
		t.Errorf("missing chunk = %d, want 404 rather than the page", rec.Code)
	}
	if rec := get(t, h, "POST", "/"); rec.Code != 405 {
		t.Errorf("POST = %d, want 405", rec.Code)
	}

	sum := sha256.Sum256([]byte("boot()"))
	csp := get(t, h, "GET", "/").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Errorf("csp %q does not allow the inline boot script by hash", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("csp %q", csp)
	}
}

func TestNoBuild(t *testing.T) {
	if rec := get(t, Handler(fstest.MapFS{}), "GET", "/"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "make ui") {
		t.Errorf("no build = %d %q", rec.Code, rec.Body.String())
	}
}
