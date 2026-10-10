// Package bbot decodes the event stream a BBOT scan writes. It is only a decoder: no
// database, no perimeter classification, no due dates. It returns the file's claims,
// deduplicated and typed, for internal/ingest to turn into inventory.
//
// The types are transcribed, not imported, like the FastRecon report: sharing a type with
// a producer turns its release notes into compile errors here, and it publishes no schema.
package bbot

import (
	"encoding/json"
	"strings"
	"time"
)

// Event types this decoder knows. Others are counted and ignored: the stream has
// no version number, a new release will emit unknown types, and refusing the
// file would turn someone else's upgrade into an outage here.
const (
	TypeScan        = "SCAN"
	TypeDNSName     = "DNS_NAME"
	TypeIPAddress   = "IP_ADDRESS"
	TypeOpenTCPPort = "OPEN_TCP_PORT"
	TypeURL         = "URL"
	TypeProtocol    = "PROTOCOL"
	TypeTechnology  = "TECHNOLOGY"
	TypeASN         = "ASN"
	TypeOrgStub     = "ORG_STUB"
)

// event is one line of the stream. The payload is a string Data or an object DataJSON,
// never both or neither, so readers branch on which arrived and not on the type name; a
// changed payload shape then cannot decode into a zero value that looks like an answer.
//
// Parent, ParentUUID, ParentChain and DiscoveryPath are absent on purpose: a third of the
// sample points at a parent never written, SCAN is its own parent and the stream is not
// time-sorted. An asset is built from the event's own host, port and payload instead.
type event struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// Data is raw, not a string. As a string, one event carrying an object fails the
	// whole line with an UnmarshalTypeError and discards its other fields under a
	// reason blaming the line's shape; a producer moving a payload from data_json
	// to data would silently shrink every import.
	Data     json.RawMessage `json:"data"`
	DataJSON json.RawMessage `json:"data_json"`

	Host string `json:"host"`
	Port int    `json:"port"`

	Module string `json:"module"`
	// Timestamp is a float unix epoch, converted in at() instead of by encoding/json.
	Timestamp float64 `json:"timestamp"`
	// Scope is BBOT's own verdict (in-scope, affiliate or distance-N). It goes to
	// lineage and is never acted on: this platform's perimeter decides what is probed.
	Scope   string   `json:"scope_description"`
	Tags    []string `json:"tags"`
	Context string   `json:"discovery_context"`
}

// text reads Data when it is a string, else empty. An unexpected payload is no
// reason to lose the event.
func (e *event) text() string {
	if len(e.Data) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(e.Data, &value); err != nil {
		return ""
	}
	return value
}

// at converts the float epoch and reports whether the file carried one.
func (e *event) at() (time.Time, bool) {
	if e.Timestamp <= 0 {
		return time.Time{}, false
	}
	sec, frac := int64(e.Timestamp), e.Timestamp-float64(int64(e.Timestamp))
	return time.Unix(sec, int64(frac*float64(time.Second))).UTC(), true
}

// scanPayload is what a SCAN event carries, minus the resolved preset (several
// kilobytes of configuration that answer nothing here).
type scanPayload struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Target struct {
		Seeds []string `json:"seeds"`
	} `json:"target"`
	StartedAt float64 `json:"started_at"`
}

// urlPayload is a URL event's object. Only the URL is read: body hashes and the
// redirect location are measurements, which imports leave out.
type urlPayload struct {
	URL string `json:"url"`
}

// protocolPayload is what fingerprintx concluded about a port.
type protocolPayload struct {
	Protocol string `json:"protocol"`
	Banner   string `json:"banner"`
}

// technologyPayload is one CPE string or product name beside its host. The host
// is read here too: these events have no data field to fall back on, and a
// producer omitting the top-level host would drop the technology.
type technologyPayload struct {
	Host       string `json:"host"`
	Technology string `json:"technology"`
}

// normalizeHost lowercases and strips the trailing dot, nothing more. The
// canonical form is normalize's; a second, weaker version here makes two
// spellings of one host two assets.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}
