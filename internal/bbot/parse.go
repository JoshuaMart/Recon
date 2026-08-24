package bbot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What each type does with what it carries, said in the answer rather than left
// to be inferred from a zero. "Seventy one URL events and two services" is a
// sentence somebody has to be able to read without opening this file.
const (
	noteCoveredByService = "the service is the asset, the path stays out"
	noteLineage          = "recorded in lineage, never measured here"
	noteProvenance       = "read for the provenance of the import"
	noteOwnEnrichment    = "this platform resolves that itself, on the address it connected to"
	noteUnknown          = "this decoder does not know the type"
)

// Scan is one file's claims.
type Scan struct {
	Provenance Provenance
	// Hosts and Services are deduplicated and ordered, so two imports of the
	// same file walk the assets in the same order and a test can assert one.
	Hosts    []Host
	Services []Service
	// Counts says what every type in the file produced, including the types
	// that produced nothing. A count of assets on its own reads as the whole
	// answer, and it is never the whole answer here.
	Counts map[string]*TypeCount
	// Refused is the lines that were not events. One bad line costs itself and
	// not the file: a stream written by a process that was killed ends in half
	// a line, and that is not a reason to lose the other four hundred.
	//
	// It stops at maxRefused entries and RefusedBeyond counts the rest, because
	// the list is diagnostic and the hundredth entry says what the first said.
	Refused       []Refused
	RefusedBeyond int
	// TypesBeyond counts the event types past maxTypes, which a real file never
	// reaches and a malformed one reaches immediately.
	TypesBeyond int
}

// Provenance is what the SCAN event said about the run that produced the file.
// Every field is empty when the file carries no SCAN event, which a truncated
// or filtered stream legitimately does not.
type Provenance struct {
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Target  []string  `json:"target,omitempty"`
	Started time.Time `json:"started_at,omitzero"`
}

// Host is a name or an address the file claims exists.
type Host struct {
	// Name is as the file spelled it, lowercased. Turning it into a canonical
	// key is normalize's job, and a refusal there is a refusal of this entry.
	Name string
	// At is the earliest sighting, which is the one that back-dates first_seen.
	At           time.Time
	Module       string
	Context      string
	Scope        string
	Technologies []string
}

// Service is a port the file claims is open.
type Service struct {
	Host     string
	Port     int
	At       time.Time
	Module   string
	Context  string
	Scope    string
	Protocol string
	Banner   string
}

// TypeCount is one event type's contribution.
//
// Hosts and Services count the distinct assets this type was the first to name,
// not the events that named them: a real file carries them at an order of
// magnitude apart, and reporting only one of the two numbers would either
// overstate what an import did or hide what it read.
//
// Neither carries omitempty, and that is the point of the structure. A type
// that produced nothing says so with a zero rather than with a missing key,
// because a reader cannot tell a key this decoder omitted from one it never
// knew about, and every type in the file appears here whether it became an
// asset or not.
type TypeCount struct {
	Seen     int    `json:"seen"`
	Hosts    int    `json:"hosts"`
	Services int    `json:"services"`
	Note     string `json:"note,omitempty"`
}

// Refused is a line that was not an event.
type Refused struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// What one file may say about itself, so that a body of rubbish cannot become
// an answer bigger than the file.
//
// Both of these bound structures the decoder builds before anything downstream
// gets to look at the asset count and refuse the import. A 64 MB body of
// one-byte lines is thirty million refusals, and reporting them would spend the
// memory twice: once to collect them and once to serialize them back to
// somebody who already knows their file is broken.
const (
	maxRefused = 100
	maxTypes   = 200
)

// Error is a refusal of the whole file, as opposed to of one line. The two get
// different answers because one is the caller's file and the other is one line
// of it, and treating a typo as an outage is the failure the assets form
// already avoids.
type Error struct{ reason string }

func (e *Error) Error() string { return e.reason }

// Parse reads the stream.
//
// It never returns a partial Scan beside an error: either the file was a stream
// of events, in which case the bad lines are in Refused, or it was not a stream
// of events at all, in which case nothing is imported.
func Parse(r io.Reader) (Scan, error) {
	s := Scan{Counts: map[string]*TypeCount{}}
	hosts := map[string]*Host{}
	services := map[string]*Service{}

	reader := bufio.NewReader(r)
	for line := 1; ; line++ {
		raw, err := reader.ReadBytes('\n')
		if len(raw) == 0 && err != nil {
			if err == io.EOF {
				break
			}
			return Scan{}, err
		}
		raw = bytes.TrimSpace(raw)

		if len(raw) > 0 {
			if refusal := s.line(raw, line, hosts, services); refusal != nil {
				return Scan{}, refusal
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return Scan{}, err
		}
	}

	if len(s.Counts) == 0 && len(s.Refused) == 0 && s.RefusedBeyond == 0 {
		return Scan{}, &Error{reason: "the body carried no events"}
	}
	s.collect(hosts, services)
	return s, nil
}

// refuse records a line that was not an event, up to the bound.
func (s *Scan) refuse(line int, reason string) {
	if len(s.Refused) >= maxRefused {
		s.RefusedBeyond++
		return
	}
	s.Refused = append(s.Refused, Refused{Line: line, Reason: reason})
}

// line decodes one line, and returns non-nil only for a fault of the file
// rather than of the line.
func (s *Scan) line(raw []byte, number int, hosts map[string]*Host, services map[string]*Service) error {
	// A leading bracket is the shape jq produces, and somebody will have piped
	// the file through jq before sending it. Naming that is worth one branch:
	// decoded line by line it is a stream of syntax errors, and the answer
	// would blame the file's content for a mistake about its container.
	if raw[0] == '[' {
		return &Error{reason: "the body is a JSON array. output.json is newline delimited JSON, one event per line"}
	}

	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		s.refuse(number, "not a JSON object")
		return nil
	}
	if e.Type == "" {
		s.refuse(number, "no event type")
		return nil
	}

	count := s.Counts[e.Type]
	if count == nil {
		if len(s.Counts) >= maxTypes {
			// Past the bound the type is still read, so its events still
			// produce their assets. Only the per type line is dropped, and the
			// count of what was dropped goes in the answer.
			s.TypesBeyond++
			count = &TypeCount{}
		} else {
			count = &TypeCount{}
			s.Counts[e.Type] = count
		}
	}
	count.Seen++

	switch e.Type {
	case TypeScan:
		count.Note = noteProvenance
		s.provenance(&e)
	case TypeDNSName, TypeIPAddress:
		// The payload is the name, and the top level host repeats it. The
		// payload is authoritative: host is absent on some producers' events
		// and the file would silently contribute nothing.
		s.host(hosts, count, &e, firstNonEmpty(e.Data, e.Host))
	case TypeOpenTCPPort:
		s.service(hosts, services, count, &e, e.Host, e.Port, "", "")
	case TypeURL:
		// A URL creates the service it sits on and never a url asset. 4.3 makes
		// that a rule about producers, and this is a producer. So the event is
		// not ignored, it is read for its host and port, and the path it came
		// with is the part that goes nowhere.
		count.Note = noteCoveredByService
		host, port := urlTarget(&e)
		s.service(hosts, services, count, &e, host, port, "", "")
	case TypeProtocol:
		var payload protocolPayload
		_ = json.Unmarshal(e.DataJSON, &payload)
		count.Note = noteLineage
		s.service(hosts, services, count, &e, e.Host, e.Port, payload.Protocol, payload.Banner)
	case TypeTechnology:
		var payload technologyPayload
		_ = json.Unmarshal(e.DataJSON, &payload)
		count.Note = noteLineage
		s.technology(hosts, count, &e, payload.Technology)
	case TypeASN, TypeOrgStub:
		count.Note = noteOwnEnrichment
	default:
		count.Note = noteUnknown
	}
	return nil
}

func (s *Scan) provenance(e *event) {
	var payload scanPayload
	if err := json.Unmarshal(e.DataJSON, &payload); err != nil {
		return
	}
	// The stream carries a SCAN at the start and another at the end, and the
	// second one is not a second scan. Whichever arrives first wins, so the
	// provenance describes the run rather than its last line.
	if s.Provenance.ID != "" {
		return
	}
	s.Provenance = Provenance{ID: payload.ID, Name: payload.Name, Target: payload.Target.Seeds}
	if payload.StartedAt > 0 {
		sec := int64(payload.StartedAt)
		s.Provenance.Started = time.Unix(sec, 0).UTC()
	}
}

// host records a name, merging with what an earlier event said about it.
func (s *Scan) host(hosts map[string]*Host, count *TypeCount, e *event, name string) *Host {
	name = normalizeHost(name)
	if name == "" {
		return nil
	}

	at, _ := e.at()
	existing := hosts[name]
	if existing == nil {
		hosts[name] = &Host{
			Name: name, At: at, Module: e.Module, Context: e.Context, Scope: e.Scope,
		}
		if count != nil {
			count.Hosts++
		}
		return hosts[name]
	}
	// The earliest sighting is the one kept, because it is the one that
	// back-dates first_seen, and the file is not sorted by time.
	if !at.IsZero() && (existing.At.IsZero() || at.Before(existing.At)) {
		existing.At, existing.Module, existing.Context = at, e.Module, e.Context
	}
	return existing
}

// service records an open port and the host under it.
//
// The host is created here too, and that is not defensive: a file can carry an
// OPEN_TCP_PORT for a name whose DNS_NAME event was filtered out of the output,
// and a service with no host is a service whose scheduling nothing carries.
func (s *Scan) service(
	hosts map[string]*Host, services map[string]*Service,
	count *TypeCount, e *event, host string, port int, protocol, banner string,
) {
	host = normalizeHost(host)
	if host == "" {
		return
	}
	// Counted against this type, not against nothing. A file can carry a port
	// for a name whose DNS_NAME event never reached the output, and a host
	// attributed to no type at all would make the per type numbers stop summing
	// to the totals beside them.
	//
	// Before the port is judged, and that order is the point: an event naming a
	// host this decoder cannot make a service out of still names the host. The
	// port is what is unusable, and dropping both would lose a real name over a
	// field that was missing.
	s.host(hosts, count, e, host)

	if port <= 0 || port > 65535 {
		return
	}

	at, _ := e.at()
	key := fmt.Sprintf("%s:%d", host, port)
	existing := services[key]
	if existing == nil {
		services[key] = &Service{
			Host: host, Port: port, At: at, Module: e.Module, Context: e.Context,
			Scope: e.Scope, Protocol: protocol, Banner: banner,
		}
		if count != nil {
			count.Services++
		}
		return
	}
	if !at.IsZero() && (existing.At.IsZero() || at.Before(existing.At)) {
		existing.At, existing.Module, existing.Context = at, e.Module, e.Context
	}
	// A PROTOCOL event arrives after the OPEN_TCP_PORT that created the row, so
	// what it concluded is filled in rather than dropped for being late.
	if protocol != "" {
		existing.Protocol = protocol
	}
	if banner != "" {
		existing.Banner = banner
	}
}

// technology attaches a product string to its host.
//
// It creates the host when nothing else has, for the same reason service does,
// and it counts under the type that named it first.
func (s *Scan) technology(hosts map[string]*Host, count *TypeCount, e *event, technology string) {
	if technology == "" {
		return
	}
	host := s.host(hosts, count, e, firstNonEmpty(e.Host, e.Data))
	if host == nil {
		return
	}
	for _, already := range host.Technologies {
		if already == technology {
			return
		}
	}
	host.Technologies = append(host.Technologies, technology)
}

// collect flattens the maps into the ordered slices callers walk.
func (s *Scan) collect(hosts map[string]*Host, services map[string]*Service) {
	s.Hosts = make([]Host, 0, len(hosts))
	for _, host := range hosts {
		sort.Strings(host.Technologies)
		s.Hosts = append(s.Hosts, *host)
	}
	sort.Slice(s.Hosts, func(i, j int) bool { return s.Hosts[i].Name < s.Hosts[j].Name })

	s.Services = make([]Service, 0, len(services))
	for _, service := range services {
		s.Services = append(s.Services, *service)
	}
	sort.Slice(s.Services, func(i, j int) bool {
		if s.Services[i].Host != s.Services[j].Host {
			return s.Services[i].Host < s.Services[j].Host
		}
		return s.Services[i].Port < s.Services[j].Port
	})
}

// Assets is how many rows an import of this scan would touch, which is what the
// bound in front of the endpoint is expressed in.
func (s Scan) Assets() int { return len(s.Hosts) + len(s.Services) }

// urlTarget is the service a URL event names.
//
// The top level host and port are read first, because the producer computes
// them and they are what every other type here uses. The URL itself is the
// fallback rather than the source: an event that carried a URL and no port
// would otherwise contribute nothing, silently, and the port a scheme implies
// is exactly the information the fallback exists to recover.
func urlTarget(e *event) (string, int) {
	if e.Host != "" && e.Port > 0 {
		return e.Host, e.Port
	}

	var payload urlPayload
	if err := json.Unmarshal(e.DataJSON, &payload); err != nil || payload.URL == "" {
		return e.Host, e.Port
	}
	parsed, err := url.Parse(payload.URL)
	if err != nil || parsed.Hostname() == "" {
		return e.Host, e.Port
	}

	host := parsed.Hostname()
	if e.Host != "" {
		host = e.Host
	}
	port := e.Port
	if port <= 0 {
		if explicit := parsed.Port(); explicit != "" {
			port, _ = strconv.Atoi(explicit)
		} else {
			switch strings.ToLower(parsed.Scheme) {
			case "https":
				port = 443
			case "http":
				port = 80
			}
		}
	}
	return host, port
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
