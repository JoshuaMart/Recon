package signals

import "strings"

// Takeover candidate kinds: two halves, one field, so the notifier and the console
// read one thing.
const (
	// KindOrphanCNAME is a name pointing at a name that no longer exists. It arrives
	// whole from a resolution: a recursive resolver follows the chain and returns
	// nxdomain with the CNAME still in the answer, so pointer and proof come in one query.
	KindOrphanCNAME = "orphan_cname"
	// KindUnclaimedService is a name pointing at a live service saying nobody owns it.
	// Invisible in DNS, since the name resolves; it reads only in the service's response.
	KindUnclaimedService = "unclaimed_service"
)

// Takeover is the structured finding, deliberately not a boolean: a flag would
// force rewriting the probe the day an alert must carry what is vulnerable and on
// what evidence. The three fields are stable across passes; the timestamp is added
// at ingestion, as one in the payload would differ every probe and defeat
// deduplication on the assets worth following.
type Takeover struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Signature string `json:"signature"`
}

// Dangling reads a resolution for a name pointing at nothing. The CNAME must be
// there: a plain nxdomain is a dead name, a death and not a finding. A takeover
// candidate still points somewhere, and that somewhere is claimable.
func Dangling(status, reason string, cnames []string) *Takeover {
	if status != "dead" || reason != "nxdomain" || len(cnames) == 0 {
		return nil
	}
	// The last hop is the one that failed to resolve: the name someone would register.
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

// Map renders a finding for a payload. A plain map because the normalizer takes
// one; the JSON encoder would be a round trip for three strings.
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
