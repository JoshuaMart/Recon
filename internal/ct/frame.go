package ct

import (
	"strings"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// Message types and entry types, as the feed spells them.
const (
	// MessageCertificate is the only message type the lite stream emits.
	MessageCertificate = "certificate_update"

	// EntryPrecert and EntryCertificate are the two halves of an issuance.
	// Both are logged and both carry the same names, which is one of the
	// reasons the same SAN set arrives more than once.
	EntryPrecert     = "PrecertLogEntry"
	EntryCertificate = "X509LogEntry"
)

// Frame is one message of the lite stream.
//
// Transcribed from the running feed rather than from its documentation, and
// pinned in testdata/stream.jsonl, because the position of a field only exists
// once a real document has been decoded: a struct written from a README
// compiles and every test built on the same struct passes.
//
// The fields that are absent are as deliberate as the ones that are here. The
// DER and the chain live on /full-stream and nothing in this project parses a
// certificate. The validity dates are on the wire and are not read: what a
// candidate is worth is decided by probing it, not by what the issuer wrote.
type Frame struct {
	MessageType string `json:"message_type"`
	Data        struct {
		UpdateType string  `json:"update_type"`
		CertIndex  int64   `json:"cert_index"`
		Seen       float64 `json:"seen"`
		LeafCert   struct {
			// The SAN list, wildcards included. It is empty on a certificate
			// that carries no DNS name at all: the feed strips the non-DNS
			// SANs itself, so an IP-only certificate arrives with this list
			// empty and the subject's CN empty beside it. Measured at 0.8 % of
			// the stream, and it is the case a decoder gets wrong first.
			AllDomains []string `json:"all_domains"`
			Issuer     struct {
				CN string `json:"CN"`
				O  string `json:"O"`
			} `json:"issuer"`
		} `json:"leaf_cert"`
		// Which log it arrived from. With the issuer, this is the whole of a
		// candidate's lineage, and it is the reason /domains-only is not what
		// this dials even though it is fifteen times smaller on the wire.
		Source struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"source"`
	} `json:"data"`
}

// Sighting is what one SAN of one certificate means to one programme.
//
// A wildcard carries no Name, because it names no host. It is counted against
// its apex and creates nothing, which is the structural blind spot rather than
// an omission: a certificate for *.target.com reveals no subdomain, and mature
// organizations use them for exactly that.
type Sighting struct {
	Claim    Claim
	Name     string
	Wildcard bool
}

// Sightings classifies the SAN list of one certificate against the set.
//
// malformed counts the SANs that could not be turned into a name at all. It is
// returned rather than logged because the honest thing to do with it is a
// counter: a name dropped for a formatting reason is a name that is never
// queried, and nothing downstream would say so.
func (s *Set) Sightings(sans []string) (found []Sighting, malformed int) {
	if s == nil || len(s.byApex) == 0 || len(sans) == 0 {
		return nil, 0
	}

	// A name repeated inside one certificate is one sighting. The subject's
	// common name is routinely also the first SAN, so this is the common case
	// rather than a defensive one.
	seen := make(map[string]struct{}, len(sans))

	for _, san := range sans {
		raw := strings.TrimSpace(san)
		if raw == "" {
			continue
		}
		if _, duplicate := seen[raw]; duplicate {
			continue
		}
		seen[raw] = struct{}{}

		base, wildcard := strings.CutPrefix(raw, "*.")

		// A wildcard is walked on what it is a wildcard of, so it is counted
		// against the apex it belongs to. Anything still carrying a star after
		// one cut is not a name this can reason about.
		if wildcard {
			host, err := normalize.Hostname(base)
			if err != nil || strings.Contains(host, "*") {
				malformed++
				continue
			}
			for _, claim := range s.Match(host) {
				found = append(found, Sighting{Claim: claim, Wildcard: true})
			}
			continue
		}

		key, err := normalize.FQDN(raw)
		if err != nil {
			malformed++
			continue
		}
		for _, claim := range s.Match(key.Host) {
			found = append(found, Sighting{Claim: claim, Name: key.Value})
		}
	}
	return found, malformed
}
