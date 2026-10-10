// Package ct turns the Certificate Transparency stream into candidate assets. The feed is
// someone else's (certstream-server-go follows the public logs and serves a websocket); here
// is the matcher, a loop in the control plane and not a service, as a second service would
// need a database credential outside the control plane or an endpoint creating assets.
//
// With millions of certificates a day, "which programmes claim this name" is answered
// without the database or a regular expression: a walk up the labels through an in-memory
// set, at most one lookup per label.
package ct

import (
	"strings"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// Claim is one programme's declaration of one apex. A name can be claimed more
// than once (two programmes may hold the same apex, or one under another), so
// claims travel as a list.
type Claim struct {
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	Apex      string
}

// Set is the apex set the stream is matched against. Read without a lock and never
// mutated: a reload builds a new one and swaps the pointer, since a map written
// during a walk gives wrong matches, not an error.
type Set struct {
	byApex map[string][]Claim
}

// NormalizeApex is the one spelling of an apex this package uses: the project's
// canonicalization, not a lowercase. A scope rule is stored as typed, and the perimeter
// engine compiles it through normalize.Hostname (IDN to punycode), as every SAN reaching the
// walk has been; lowercasing would leave an IDN apex in Unicode, matching nothing in a set
// that claims to hold it. The counters key on it too, so a set and its rows cannot spell an
// apex two ways.
func NormalizeApex(pattern string) (string, bool) {
	host, err := normalize.Hostname(strings.TrimSpace(pattern))
	if err != nil || host == "" {
		return "", false
	}
	return host, true
}

// NewSet compiles the claims into the set the walk reads. A claim whose apex is
// not a usable name is dropped; the perimeter refuses it too.
func NewSet(claims []Claim) *Set {
	byApex := make(map[string][]Claim, len(claims))
	for _, claim := range claims {
		apex, ok := NormalizeApex(claim.Apex)
		if !ok {
			continue
		}
		claim.Apex = apex
		byApex[apex] = append(byApex[apex], claim)
	}
	return &Set{byApex: byApex}
}

// Apexes is how many distinct apexes the set holds.
func (s *Set) Apexes() int {
	if s == nil {
		return 0
	}
	return len(s.byApex)
}

// Match walks the labels of host and returns every claim on the path. It climbs label
// boundaries, not a string suffix, so target.com.evil.com matches nothing (a suffix test
// would let anyone enter another's perimeter by registering a domain). It does not stop at
// the first match: an apex may sit under another programme's apex. The verdict is exactly
// scope's apex rule, asserted by tests, since a matcher disagreeing with the perimeter
// engine would be a second perimeter engine.
func (s *Set) Match(host string) []Claim {
	if s == nil || len(s.byApex) == 0 {
		return nil
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))

	var found []Claim
	for name != "" {
		found = append(found, s.byApex[name]...)

		dot := strings.IndexByte(name, '.')
		if dot < 0 {
			break
		}
		name = name[dot+1:]
	}
	return found
}
