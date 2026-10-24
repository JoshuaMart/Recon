package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SessionCookie is the cookie the UI holds after logging in.
const SessionCookie = "recon_session"

// sessionTTL is how long a login lasts.
const sessionTTL = 30 * 24 * time.Hour

// Auth checks the API key, as a bearer token or through a signed session
// cookie. The cookie is signed with a key derived from the API key, so
// changing the API key ends every session.
type Auth struct {
	apiKey     []byte
	signingKey []byte
	origin     string // scheme://host[:port] of public_url
	secure     bool
	now        func() time.Time
}

// NewAuth builds the checker for apiKey, with publicURL as the only origin a
// cookie-authenticated write may come from.
func NewAuth(apiKey, publicURL string) (*Auth, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("recon session v1"))
	return &Auth{
		apiKey:     []byte(apiKey),
		signingKey: mac.Sum(nil),
		origin:     u.Scheme + "://" + u.Host,
		secure:     u.Scheme == "https",
		now:        time.Now,
	}, nil
}

func (a *Auth) keyMatches(k string) bool {
	return subtle.ConstantTimeCompare([]byte(k), a.apiKey) == 1
}

func (a *Auth) sign(exp int64) string {
	mac := hmac.New(sha256.New, a.signingKey)
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) issue() (string, time.Time) {
	exp := a.now().Add(sessionTTL)
	e := exp.Unix()
	return strconv.FormatInt(e, 10) + "." + a.sign(e), exp
}

func (a *Auth) sessionValid(v string) bool {
	expStr, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || a.now().Unix() >= exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(a.sign(exp)))
}

// Require lets a request through when it carries the key or a valid session.
// A write authenticated by the cookie must also come from public_url's origin.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || !a.keyMatches(token) {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "missing, wrong or expired credential")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(SessionCookie)
		if err != nil || !a.sessionValid(c.Value) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing, wrong or expired credential")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != a.origin {
			writeError(w, http.StatusForbidden, "bad_origin", "a write with the session cookie must come from "+a.origin)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Login exchanges the API key for a session cookie.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "expected {\"key\": \"...\"}")
		return
	}
	if !a.keyMatches(body.Key) {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "wrong key")
		return
	}
	v, exp := a.issue()
	// Secure follows public_url: a local http deployment cannot hold a Secure cookie.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name: SessionCookie, Value: v, Path: "/", Expires: exp,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Logout clears the cookie.
func (a *Auth) Logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows public_url, as in Login
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
