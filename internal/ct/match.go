// Package ct turns the Certificate Transparency stream into candidates: a
// name under a known wildcard, seen in a certificate minutes after it was
// issued, enters the CT lane to be probed at once.
package ct

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/canon"
)

// Certificate is what the matcher reads of one stream message.
type Certificate struct {
	Names  []string
	Issuer string
	Log    string
	Seen   time.Time
}

// message is the lite certstream format: only the fields read here.
type message struct {
	Type string `json:"message_type"`
	Data struct {
		Leaf struct {
			Domains []string `json:"all_domains"`
			Issuer  struct {
				O  *string `json:"O"`
				CN *string `json:"CN"`
			} `json:"issuer"`
		} `json:"leaf_cert"`
		Seen   float64 `json:"seen"`
		Source struct {
			Name string `json:"name"`
		} `json:"source"`
	} `json:"data"`
}

// errNotCertificate is a message that carries no certificate: a heartbeat.
var errNotCertificate = errors.New("not a certificate update")

// Parse reads one stream message.
func Parse(b []byte) (Certificate, error) {
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		return Certificate{}, err
	}
	if m.Type != "certificate_update" {
		return Certificate{}, errNotCertificate
	}
	var issuer []string
	for _, p := range []*string{m.Data.Leaf.Issuer.O, m.Data.Leaf.Issuer.CN} {
		if p != nil && *p != "" {
			issuer = append(issuer, *p)
		}
	}
	sec := int64(m.Data.Seen)
	return Certificate{
		Names: m.Data.Leaf.Domains, Issuer: strings.Join(issuer, " "), Log: m.Data.Source.Name,
		Seen: time.Unix(sec, int64((m.Data.Seen-float64(sec))*1e9)).UTC(),
	}, nil
}

// Target is a wildcard a name falls under.
type Target struct {
	WildcardID uuid.UUID
	ProgramID  uuid.UUID
	Apex       string
}

// Index finds the wildcards a name falls under.
type Index map[string][]Target

// Add puts a wildcard in the index.
func (ix Index) Add(t Target) { ix[t.Apex] = append(ix[t.Apex], t) }

// Match walks a name's label boundaries up to its apexes: staging.api.t.com,
// then api.t.com, then t.com. Walking labels rather than testing a suffix is
// what keeps t.com.evil.com away from t.com. Every apex on the way counts, so
// a name under two programs is matched twice. A wildcard SAN (*.t.com) only
// says which wildcards it covers: wildcard is true and the name is not one to
// create.
func (ix Index) Match(san string) (name string, targets []Target, wildcard bool) {
	s := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(san)), ".")
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s, wildcard = rest, true
	}
	// The walk runs on every name of every certificate: the name is made
	// canonical only once it falls under an apex.
	for cur := s; ; {
		targets = append(targets, ix[cur]...)
		_, parent, ok := strings.Cut(cur, ".")
		if !ok || !strings.Contains(parent, ".") {
			break
		}
		cur = parent
	}
	if len(targets) == 0 {
		return "", nil, wildcard
	}
	n, err := canon.FQDN(s)
	if err != nil {
		return "", nil, wildcard
	}
	return n, targets, wildcard
}
