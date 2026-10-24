package runs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Purpose is what a run token may be used for. It is signed, so a targets
// token never posts a report.
type Purpose string

// The two things a job does with Recon.
const (
	PurposeTargets Purpose = "targets"
	PurposeReport  Purpose = "report"
)

// ErrBadToken is a token that is missing, malformed, signed by another key,
// for another purpose or another run, or expired. The caller answers all of
// them alike.
var ErrBadToken = errors.New("bad run token")

// Tokens mints and checks the credentials a job carries: an HMAC over run,
// purpose and expiry. Nothing is stored, so nothing is revoked or purged; the
// expiry ends a token. The key is derived from the API key, so changing it
// ends every token in flight.
type Tokens struct{ key []byte }

// NewTokens derives the signing key from the API key.
func NewTokens(apiKey string) *Tokens {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("recon run token v1"))
	return &Tokens{key: mac.Sum(nil)}
}

func (t *Tokens) sign(body string) string {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint returns a token for one run, one purpose, until expires.
func (t *Tokens) Mint(p Purpose, run uuid.UUID, expires time.Time) string {
	body := string(p) + "." + run.String() + "." + strconv.FormatInt(expires.Unix(), 10)
	return body + "." + t.sign(body)
}

// Check verifies a token for this run and purpose at now.
func (t *Tokens) Check(token string, p Purpose, run uuid.UUID, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return ErrBadToken
	}
	body := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(parts[3]), []byte(t.sign(body))) {
		return ErrBadToken
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || Purpose(parts[0]) != p || parts[1] != run.String() || now.Unix() >= exp {
		return ErrBadToken
	}
	return nil
}
