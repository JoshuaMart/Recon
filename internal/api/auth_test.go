package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef"

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth(testKey, "https://recon.example")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func protected(a *Auth) http.Handler {
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
}

func login(t *testing.T, a *Auth, key string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/session", strings.NewReader(`{"key":"`+key+`"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	return nil
}

func call(t *testing.T, h http.Handler, method string, mutate func(*http.Request)) int {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, "/api/programs", nil)
	if mutate != nil {
		mutate(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestBearer(t *testing.T) {
	h := protected(newTestAuth(t))
	if got := call(t, h, http.MethodPost, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testKey) }); got != http.StatusTeapot {
		t.Errorf("good key = %d", got)
	}
	for name, header := range map[string]string{"wrong key": "Bearer nope", "not bearer": "Basic " + testKey} {
		if got := call(t, h, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", header) }); got != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, got)
		}
	}
	if got := call(t, h, http.MethodGet, nil); got != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", got)
	}
}

func TestSessionCookie(t *testing.T) {
	a := newTestAuth(t)
	h := protected(a)
	if login(t, a, "wrong") != nil {
		t.Fatal("a wrong key must not get a cookie")
	}
	c := login(t, a, testKey)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", c)
	}

	withCookie := func(origin string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(c)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		}
	}
	if got := call(t, h, http.MethodGet, withCookie("")); got != http.StatusTeapot {
		t.Errorf("read with cookie = %d", got)
	}
	if got := call(t, h, http.MethodPost, withCookie("https://recon.example")); got != http.StatusTeapot {
		t.Errorf("write from our origin = %d", got)
	}
	if got := call(t, h, http.MethodPost, withCookie("https://evil.example")); got != http.StatusForbidden {
		t.Errorf("write from another origin = %d, want 403", got)
	}
	if got := call(t, h, http.MethodPost, withCookie("")); got != http.StatusForbidden {
		t.Errorf("write without an origin = %d, want 403", got)
	}

	tampered := *c
	tampered.Value = strings.Replace(c.Value, ".", "9.", 1)
	if got := call(t, h, http.MethodGet, func(r *http.Request) { r.AddCookie(&tampered) }); got != http.StatusUnauthorized {
		t.Errorf("tampered expiry = %d, want 401", got)
	}

	a.now = func() time.Time { return time.Now().Add(sessionTTL + time.Hour) }
	if got := call(t, h, http.MethodGet, withCookie("")); got != http.StatusUnauthorized {
		t.Errorf("expired session = %d, want 401", got)
	}
}

func TestChangingTheKeyEndsSessions(t *testing.T) {
	c := login(t, newTestAuth(t), testKey)
	other, err := NewAuth(strings.Repeat("x", 32), "https://recon.example")
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, protected(other), http.MethodGet, func(r *http.Request) { r.AddCookie(c) }); got != http.StatusUnauthorized {
		t.Errorf("old session under a new key = %d, want 401", got)
	}
}
