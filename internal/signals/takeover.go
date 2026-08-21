package signals

import "strings"

// Takeover candidate kinds. Two halves, one field, because the notifier and the
// console have one thing to read either way.
const (
	// KindOrphanCNAME is a name pointing at a name that no longer exists. It
	// arrives whole from a resolution: a recursive resolver follows the chain
	// itself and returns nxdomain with the CNAME still in the answer section,
	// so the dangling pointer and its proof come back in one query.
	KindOrphanCNAME = "orphan_cname"
	// KindUnclaimedService is a name pointing at a live service that says
	// nobody owns it. It is invisible in DNS, because the name resolves
	// perfectly well, and reads only in the service's own response.
	KindUnclaimedService = "unclaimed_service"
)

// Takeover is the structured finding, and it is deliberately not a boolean.
//
// A bare flag would have forced rewriting the probe the day an alert has to
// carry what is vulnerable and on what evidence. The three fields here are the
// ones that are stable across passes; the timestamp is added at ingestion,
// because a date inside the payload would differ on every probe and defeat
// deduplication on exactly the assets worth following.
type Takeover struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Signature string `json:"signature"`
}

// Dangling reads a resolution for a name pointing at nothing.
//
// The CNAME has to be there. A plain nxdomain is a dead name, which is a death
// and not a finding; what makes it a takeover candidate is that the name still
// points somewhere, and that somewhere is claimable.
func Dangling(status, reason string, cnames []string) *Takeover {
	if status != "dead" || reason != "nxdomain" || len(cnames) == 0 {
		return nil
	}
	// The last hop is the one that failed to resolve, and it is the name
	// somebody would register.
	target := strings.TrimSuffix(strings.ToLower(cnames[len(cnames)-1]), ".")
	if target == "" {
		return nil
	}
	return &Takeover{Kind: KindOrphanCNAME, Target: target, Signature: "nxdomain"}
}

// Unclaimed reads a response for a service nobody has claimed.
func Unclaimed(url string, v Verdict) *Takeover {
	if v.Unclaimed == "" {
		return nil
	}
	return &Takeover{Kind: KindUnclaimedService, Target: url, Signature: v.Unclaimed}
}

// Map renders a finding for a payload. It is a plain map because that is what
// the normalizer takes, and going through the JSON encoder for three strings
// would be a round trip for nothing.
func (t *Takeover) Map() map[string]any {
	if t == nil {
		return nil
	}
	return map[string]any{
		"kind":      t.Kind,
		"target":    t.Target,
		"signature": t.Signature,
	}
}
