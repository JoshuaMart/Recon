// Package auth resolves who is asking.
//
// Every request goes through one place that produces a principal, even while
// there is one kind of caller and one role. Checks scattered inline would force
// an audit of every endpoint the day a second role appears, and that audit is
// the thing nobody does.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Action is what a caller is allowed to do. They are separate values rather
// than one "authenticated" flag because a run holds exactly one of them: a
// scanner that could also schedule work would be able to spend a programme's
// budget on targets it chose.
type Action string

const (
	// ActionIngest writes one report, for the run it names and no other.
	ActionIngest Action = "ingest"
	// ActionReadAssets reads the inventory. A run never holds it: everything a
	// run needs is in its definition, so a compromised one cannot exfiltrate a
	// perimeter.
	ActionReadAssets Action = "read_assets"
	// ActionManageScope edits programmes and rules, and enters assets by hand.
	ActionManageScope Action = "manage_scope"
	// ActionManageJobs starts runs and schedules renders.
	ActionManageJobs Action = "manage_jobs"
)

// Principal is who the request turned out to be.
type Principal struct {
	OrgID uuid.UUID
	// ActorID is the person, when there is one. A run has none, and the
	// difference has to stay visible: an attribution column believed to be
	// populated is worse than an absent one.
	ActorID *uuid.UUID
	// RunID binds an ingestion to one execution. Zero on anything else.
	RunID   uuid.UUID
	Actions []Action
}

// Can reports whether the principal holds an action.
func (p Principal) Can(action Action) bool {
	for _, held := range p.Actions {
		if held == action {
			return true
		}
	}
	return false
}

// Errors a caller can tell apart. A missing credential and a wrong one are the
// same answer to the outside and different things to a log.
var (
	ErrMissing = errors.New("no credential")
	ErrInvalid = errors.New("invalid credential")
	ErrExpired = errors.New("credential expired")
)

// MinKeyLength is what the signing key has to reach.
//
// A short key is not a weak configuration, it is an unsigned one: an HMAC whose
// key can be guessed lets anyone mint a token for any run.
const MinKeyLength = 32

// Signer mints and verifies the credentials a run carries.
//
// Both are HMACs over the run, the purpose and an expiry, so there is nothing
// to store, nothing to revoke and nothing to purge, and the expiry is
// intrinsic. Revocation is the run's state: a run in a terminal state rejects
// any later report bearing its id, so the token stays valid and stops being
// useful.
type Signer struct{ key []byte }

// NewSigner refuses a key too short to be one.
func NewSigner(key string) (*Signer, error) {
	if len(key) < MinKeyLength {
		return nil, fmt.Errorf("signing key must be at least %d bytes, got %d", MinKeyLength, len(key))
	}
	return &Signer{key: []byte(key)}, nil
}

// Purpose is what a token may be used for. It is inside the signature, so a
// token minted to fetch a target list cannot be replayed to post a report.
type Purpose string

const (
	// PurposeReport authorizes posting one run's report.
	PurposeReport Purpose = "report"
	// PurposeTargets authorizes fetching one run's frozen target list.
	PurposeTargets Purpose = "targets"
)

const tokenVersion = "v1"

// Mint returns a token bound to one run, one purpose and one expiry.
func (s *Signer) Mint(purpose Purpose, runID uuid.UUID, expires time.Time) string {
	body := s.body(purpose, runID, expires)
	return body + "." + s.sign(body)
}

// Verify checks a token and returns what it names.
func (s *Signer) Verify(purpose Purpose, token string, now time.Time) (uuid.UUID, error) {
	if strings.TrimSpace(token) == "" {
		return uuid.Nil, ErrMissing
	}

	cut := strings.LastIndex(token, ".")
	if cut < 0 {
		return uuid.Nil, ErrInvalid
	}
	body, signature := token[:cut], token[cut+1:]

	// Constant time, because a comparison that returns early tells an attacker
	// how much of a forgery was right.
	if subtle.ConstantTimeCompare([]byte(signature), []byte(s.sign(body))) != 1 {
		return uuid.Nil, ErrInvalid
	}

	parts := strings.Split(body, ".")
	if len(parts) != 4 || parts[0] != tokenVersion || parts[1] != string(purpose) {
		// The purpose is checked after the signature so that a token minted
		// for another one is a refusal rather than an oracle.
		return uuid.Nil, ErrInvalid
	}

	runID, err := uuid.Parse(parts[2])
	if err != nil {
		return uuid.Nil, ErrInvalid
	}
	seconds, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return uuid.Nil, ErrInvalid
	}
	if now.After(time.Unix(seconds, 0)) {
		return uuid.Nil, ErrExpired
	}
	return runID, nil
}

func (s *Signer) body(purpose Purpose, runID uuid.UUID, expires time.Time) string {
	return strings.Join([]string{
		tokenVersion, string(purpose), runID.String(), strconv.FormatInt(expires.Unix(), 10),
	}, ".")
}

func (s *Signer) sign(body string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Hash is how a credential is stored and looked up.
//
// Only the hash lives in the database, so a token is printed once and never
// recoverable. A plain SHA-256 rather than a password hash is right here and
// would be wrong for a password: this value is generated with full entropy, so
// there is no dictionary to slow down, and the lookup is on the hot path of
// every console request.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return sum[:]
}

// Actions turns the stored scopes into what a caller may do.
//
// An unknown scope is dropped rather than rejected. The list grows, and a token
// issued by a newer release must not lock its holder out of everything it does
// hold.
func Actions(scopes []string) []Action {
	known := map[string]Action{
		string(ActionIngest):      ActionIngest,
		string(ActionReadAssets):  ActionReadAssets,
		string(ActionManageScope): ActionManageScope,
		string(ActionManageJobs):  ActionManageJobs,
	}
	out := make([]Action, 0, len(scopes))
	for _, scope := range scopes {
		if action, ok := known[strings.TrimSpace(scope)]; ok {
			out = append(out, action)
		}
	}
	return out
}
