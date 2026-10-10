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

// What each type produced, said in the answer instead of inferred from a zero:
// "seventy-one URL events and two services" must be readable without this file.
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
	// Hosts and Services are deduplicated and ordered, so two imports of one file
	// walk the assets identically.
	Hosts    []Host
	Services []Service
	// Counts says what every type in the file produced, including those that
	// produced nothing. An asset count alone reads as the whole answer.
	Counts map[string]*TypeCount
	// Refused is the lines that were not events. A bad line costs itself, not the
	// file: a killed process leaves half a line, no reason to lose the other four
	// hundred. It stops at maxRefused and RefusedBeyond counts the rest, since the
	// hundredth entry says what the first did.
	Refused       []Refused
	RefusedBeyond int
	// TypesBeyond counts event types past maxTypes: never reached by a real file,
	// immediately by a malformed one.
	TypesBeyond int
	// beyond keeps TypesBeyond a count of types. Not reported.
	beyond map[string]struct{}
}

// Provenance is what the SCAN event said about the run that produced the file.
// Empty when the file has no SCAN event, as a truncated or filtered stream
// legitimately has none.
type Provenance struct {
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Target  []string  `json:"target,omitempty"`
	Started time.Time `json:"started_at,omitzero"`
}

// Host is a name or an address the file claims exists.
type Host struct {
	// Name is as the file spelled it, lowercased. Canonical keys are normalize's
	// job, and a refusal there refuses this entry.
	Name string
	// At is the earliest sighting, which back-dates first_seen.
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

// TypeCount is one event type's contribution. Hosts and Services count the distinct assets
// this type was first to name, not events (a real file has them an order of magnitude
// apart). Neither has omitempty: a type that produced nothing says so with a zero, since an
// omitted key cannot be told from a type the decoder never knew.
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

// What one file may say about itself, so rubbish cannot become an answer bigger
// than the file. Both bound structures the decoder builds before downstream code
// can refuse the import on asset count: a 64 MB body of one-byte lines is thirty
// million refusals, which would cost memory twice (collecting, then serializing
// them back to someone who knows the file is broken).
const (
	maxRefused = 100
	maxTypes   = 200
)

// Error is a refusal of the whole file, as opposed to one line. They get
// different answers: one is the caller's file, the other a line of it.
type Error struct{ reason string }

func (e *Error) Error() string { return e.reason }

// Parse reads the stream. It never returns a partial Scan beside an error:
// either the file was a stream of events (bad lines in Refused) or it was not
// and nothing is imported.
func Parse(r io.Reader) (Scan, error) {
	s := Scan{Counts: map[string]*TypeCount{}, beyond: map[string]struct{}{}}
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

// line decodes one line, returning non-nil only for a fault of the file, not of
// the line.
func (s *Scan) line(raw []byte, number int, hosts map[string]*Host, services map[string]*Service) error {
	// A leading bracket is jq's output shape, and people pipe files through jq
	// first. Naming it takes one branch; line by line it is a stream of syntax
	// errors that blames the content for a container mistake.
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
			// Past the bound the type is still read and its events still produce assets;
			// only the per-type line is dropped and counted in the answer.
			//
			// Counted once per type, as the field is named. Counting events would be an
			// order of magnitude out on exactly the malformed body the bound exists for.
			if _, already := s.beyond[e.Type]; !already {
				s.beyond[e.Type] = struct{}{}
				s.TypesBeyond++
			}
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
		// The payload is the name; the top-level host repeats it. The payload is
		// authoritative, as some producers omit host and the file would contribute
		// nothing.
		s.host(hosts, count, &e, firstNonEmpty(e.text(), e.Host))
	case TypeOpenTCPPort:
		s.service(hosts, services, count, &e, e.Host, e.Port, "", "")
	case TypeURL:
		// A URL creates the service it sits on, never a url asset (a rule about
		// producers, and this is one). The event is read for its host and port; the
		// path goes nowhere.
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
		s.technology(hosts, count, &e, payload.Host, payload.Technology)
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
	// A SCAN opens and another closes the stream, and the second is not a second
	// scan. The first wins, so provenance describes the run, not its last line.
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
	// The earliest sighting is kept whole: module, sentence and scope verdict go in
	// one lineage object, since mixing events would describe a sighting that never
	// happened. The file is not sorted by time, so that is reachable.
	if !at.IsZero() && (existing.At.IsZero() || at.Before(existing.At)) {
		existing.At, existing.Module, existing.Context = at, e.Module, e.Context
		existing.Scope = e.Scope
	}
	return existing
}

// service records an open port and the host under it. The host is created too,
// not defensively: a file can carry an OPEN_TCP_PORT whose DNS_NAME was
// filtered out, and a service with no host has nothing to carry its scheduling.
func (s *Scan) service(
	hosts map[string]*Host, services map[string]*Service,
	count *TypeCount, e *event, host string, port int, protocol, banner string,
) {
	host = normalizeHost(host)
	if host == "" {
		return
	}
	// Counted against this type, so a host attributed to no type does not make the
	// per-type numbers stop summing to the totals.
	//
	// Before judging the port: an event naming a host this decoder cannot make a
	// service of still names the host. Only the port is unusable, and dropping both
	// would lose a real name over a missing field.
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
		existing.Scope = e.Scope
	}
	// A PROTOCOL event arrives after the OPEN_TCP_PORT that created the row, so its
	// conclusion is filled in, not dropped for being late.
	if protocol != "" {
		existing.Protocol = protocol
	}
	if banner != "" {
		existing.Banner = banner
	}
}

// technology attaches a product string to its host, creating the host when
// nothing else has (as service does) and counting under the type that named it
// first.
func (s *Scan) technology(
	hosts map[string]*Host, count *TypeCount, e *event, named, technology string,
) {
	if technology == "" {
		return
	}
	host := s.host(hosts, count, e, firstNonEmpty(e.Host, named, e.text()))
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

// Assets is how many rows importing this scan would touch, the unit of the
// endpoint's bound.
func (s Scan) Assets() int { return len(s.Hosts) + len(s.Services) }

// urlTarget is the service a URL event names. The top-level host and port come
// first: the producer computes them and every other type uses them. The URL is
// the fallback, so an event with a URL and no port still recovers the scheme's
// implied port instead of contributing nothing.
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
