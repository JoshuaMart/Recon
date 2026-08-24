// Package bbot decodes the event stream a BBOT scan writes.
//
// It is a decoder and nothing else: it never touches the database, never
// classifies against a perimeter and never decides a due date. What it returns
// is the file's claims, deduplicated and typed, for internal/ingest to turn
// into inventory on the terms 7.6 sets.
//
// The types below are transcribed rather than imported, like the FastRecon
// report next door and for the same reason. Sharing a type with a producer
// makes its release notes into this repository's compile errors, and this
// producer publishes no schema at all.
package bbot

import (
	"encoding/json"
	"strings"
	"time"
)

// Event types this decoder knows. Anything else is counted and ignored, which
// is the rule the stream's lack of a version number forces: a new release will
// emit something not on this list, and refusing the file for it would make an
// upgrade somebody else performed into an outage here.
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

// event is one line of the stream.
//
// The payload is a string Data or an object DataJSON, never both and never
// neither, so every reader below branches on which one arrived rather than on
// the type name. That is what keeps a type whose payload shape changed from
// being decoded into a zero value that looks like a real answer.
//
// Parent, ParentUUID, ParentChain and DiscoveryPath are deliberately absent.
// A third of the sample's events point at a parent that was never written to
// the file, SCAN is its own parent, and the stream is not sorted by time. None
// of that has to be survived if the chain is never walked, and an asset is
// built from the event's own host, port and payload instead.
type event struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// Data is raw rather than a string, and that is not laziness. Typed as a
	// string, one event carrying an object here fails the whole line with an
	// UnmarshalTypeError, and every other field it held is thrown away with it
	// under a reason that blames the shape of the line. A producer moving one
	// payload from data_json to data would then shrink every import silently,
	// which is the drift this package exists to survive.
	Data     json.RawMessage `json:"data"`
	DataJSON json.RawMessage `json:"data_json"`

	Host string `json:"host"`
	Port int    `json:"port"`

	Module string `json:"module"`
	// Timestamp is a float unix epoch rather than a date, which is why it is
	// decoded as a number and converted here instead of by encoding/json.
	Timestamp float64 `json:"timestamp"`
	// Scope is BBOT's own verdict, one of in-scope, affiliate or distance-N.
	// It is recorded in lineage and never acted on: the perimeter that decides
	// what gets probed is this platform's, and 7.6 says why.
	Scope   string   `json:"scope_description"`
	Tags    []string `json:"tags"`
	Context string   `json:"discovery_context"`
}

// text reads Data when it is a string, and answers empty for anything else.
// A payload this decoder did not expect is not a reason to lose the event.
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

// at converts the float epoch, and reports whether the file carried one at all.
func (e *event) at() (time.Time, bool) {
	if e.Timestamp <= 0 {
		return time.Time{}, false
	}
	sec, frac := int64(e.Timestamp), e.Timestamp-float64(int64(e.Timestamp))
	return time.Unix(sec, int64(frac*float64(time.Second))).UTC(), true
}

// scanPayload is what a SCAN event carries, minus the resolved preset, which is
// several kilobytes of somebody's configuration and answers nothing here.
type scanPayload struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Target struct {
		Seeds []string `json:"seeds"`
	} `json:"target"`
	StartedAt float64 `json:"started_at"`
}

// urlPayload is a URL event's object. Only the URL is read: the body hashes and
// the redirect location are measurements, and 7.6 keeps measurements out.
type urlPayload struct {
	URL string `json:"url"`
}

// protocolPayload is what fingerprintx concluded about a port.
type protocolPayload struct {
	Protocol string `json:"protocol"`
	Banner   string `json:"banner"`
}

// technologyPayload is one CPE string or product name, beside the host it was
// found on. The host is read here as well as at the top level, because these
// events are the ones with no data field to fall back on: without it a producer
// that omits the top level host drops the technology and counts nothing.
type technologyPayload struct {
	Host       string `json:"host"`
	Technology string `json:"technology"`
}

// normalizeHost lowercases and strips the trailing dot, which is all this layer
// does to a name. The canonical form is normalize's business and applying a
// second, weaker version of it here is how two spellings of one host become two
// assets.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}
