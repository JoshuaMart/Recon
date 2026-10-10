package ct

import (
	"strings"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// Message types and entry types, as the feed spells them.
const (
	// MessageCertificate is the only message type the lite stream emits.
	MessageCertificate = "certificate_update"

	// EntryPrecert and EntryCertificate are the two halves of an issuance. Both are
	// logged with the same names, one reason a SAN set arrives more than once.
	EntryPrecert     = "PrecertLogEntry"
	EntryCertificate = "X509LogEntry"
)

// Frame is one message of the lite stream, transcribed from the running feed rather than its
// documentation and pinned in testdata/stream.jsonl: a field's position exists only once a
// real document is decoded, and a struct written from a README compiles with every test on
// it passing. Absent on purpose: the DER and chain (/full-stream; nothing here parses a
// certificate) and the validity dates (a candidate's worth is decided by probing).
type Frame struct {
	MessageType string `json:"message_type"`
	Data        struct {
		UpdateType string  `json:"update_type"`
		CertIndex  int64   `json:"cert_index"`
		Seen       float64 `json:"seen"`
		LeafCert   struct {
			// The SAN list, wildcards included. Empty for a certificate with no DNS name: the
			// feed strips non-DNS SANs, so an IP-only certificate arrives with this and the
			// subject CN empty. 0.8 % of the stream, and the case a decoder gets wrong first.
			AllDomains []string `json:"all_domains"`
			Issuer     struct {
				CN string `json:"CN"`
				O  string `json:"O"`
			} `json:"issuer"`
		} `json:"leaf_cert"`
		// Which log it came from. With the issuer it is a candidate's whole lineage, and
		// why this dials the lite stream and not /domains-only, fifteen times smaller.
		Source struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"source"`
	} `json:"data"`
}

// Sighting is what one SAN of one certificate means to one programme. A wildcard
// carries no Name since it names no host: it counts against its apex and creates
// nothing. That is the structural blind spot, not an omission: *.target.com
// reveals no subdomain, and mature organizations use them for that.
type Sighting struct {
	Claim    Claim
	Name     string
	Wildcard bool
}

// Sightings classifies one certificate's SAN list against the set. malformed
// counts SANs that could not become a name, returned for a counter: such a name is
// never queried and nothing downstream would say so.
func (s *Set) Sightings(sans []string) (found []Sighting, malformed int) {
	if s == nil || len(s.byApex) == 0 || len(sans) == 0 {
		return nil, 0
	}

	// A name repeated inside one certificate is one sighting; the CN is routinely also
	// the first SAN, so this is the common case.
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

		// A wildcard is walked on what it wildcards, counted against its apex. Anything
		// still starred after one cut is not a name this can reason about.
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
