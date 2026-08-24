// Package ct turns the Certificate Transparency stream into candidate assets.
//
// The feed itself is somebody else's: certstream-server-go follows the public
// logs and serves them as a websocket. What lives here is the matcher, and it
// is a loop in the control plane rather than a service of its own, because a
// second service would need either a database credential outside the control
// plane or an endpoint that creates assets, and both are boundaries this
// project spends its length holding.
//
// The stream carries several million certificates a day, so the question
// "which programmes claim this name" has to be answered without touching the
// database and without a regular expression. It is a walk up the labels through
// an in-memory set: at most as many lookups as the name has labels, whatever
// the number of programmes.
package ct

import (
	"strings"

	"github.com/google/uuid"
)

// Claim is one programme's declaration of one apex.
//
// A name can be claimed more than once. Two programmes may legitimately hold
// the same apex, and one apex may sit under another, so this travels as a list
// everywhere below rather than as a single owner.
type Claim struct {
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	Apex      string
}

// Set is the apex set the stream is matched against.
//
// It is read without a lock and never mutated: a reload builds a new one and
// swaps the pointer. A map being written while the walk reads it is the one
// fault in this loop that would produce wrong matches instead of an error.
type Set struct {
	byApex map[string][]Claim
}

// NewSet compiles the claims into the set the walk reads.
//
// Apexes arrive already normalized, because they are `scope_rule` patterns and
// those were normalized when the rule was written. Lowercasing again here is
// cheap and it means a caller assembling a set by hand cannot produce one that
// silently matches nothing.
func NewSet(claims []Claim) *Set {
	byApex := make(map[string][]Claim, len(claims))
	for _, claim := range claims {
		apex := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(claim.Apex), "."))
		if apex == "" {
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

// Match walks the labels of host and returns every claim on the path.
//
// Two properties, and both are load bearing.
//
// It climbs label boundaries rather than testing a string suffix, which is why
// target.com.evil.com matches nothing. A suffix test would let anybody put a
// name inside somebody else's perimeter by registering a domain, and it is the
// same distinction the search chapter draws when it refuses to turn a suffix
// query into a notion of domain membership.
//
// It does not stop at the first match. An apex may sit under another
// programme's apex, and stopping would drop the outer one in silence, which is
// the same rule as two programmes holding the same apex reached from the other
// end.
//
// The verdict is exactly scope's apex rule: the apex itself, or anything under
// it. That equivalence is asserted rather than intended, because a matcher that
// disagreed with the perimeter engine would be a second perimeter engine.
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
