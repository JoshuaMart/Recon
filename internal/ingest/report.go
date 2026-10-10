// Package ingest turns a scanner's report into inventory. Every conclusion is
// re-derived here: a scanner is untrusted, so what it says is evidence, and the
// scope, outcome and implied assets are decided in this package.
package ingest

import "time"

// Report is the scanner's document, schema 1.0. It is transcribed, not imported:
// the scanner ships on its own cycle and a shared type would make its refactor an
// outage here.
type Report struct {
	SchemaVersion string     `json:"schema_version"`
	Run           RunInfo    `json:"run"`
	Sources       []Source   `json:"sources"`
	Stats         Stats      `json:"stats"`
	Hosts         []Host     `json:"hosts"`
	Excluded      []Excluded `json:"excluded,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
}

// RunInfo is the execution's own metadata.
type RunInfo struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	// Input is "domain" or "targets" and says whether a missing host means anything:
	// enumeration is authoritative on presence, never absence.
	Input              string    `json:"input"`
	Scope              string    `json:"scope"`
	Stages             []string  `json:"stages"`
	Started            time.Time `json:"started_at"`
	Finished           time.Time `json:"finished_at"`
	DurationMS         int64     `json:"duration_ms"`
	Completed          bool      `json:"completed"`
	TruncatedByTimeout bool      `json:"truncated_by_timeout"`
	Version            string    `json:"version"`
	Environment        string    `json:"environment"`
	// Degraded names what this run could not guarantee. A degraded run produces no
	// informative failure: a resolver pool that could not be validated turns dead
	// hosts live or live hosts into timeouts, the two signals a death is read from.
	//
	// It sits inside the run, as the scanner puts it. A test building this structure
	// in Go cannot see the difference; only decoding a real document does, which is
	// what the round trip test is for.
	Degraded []string `json:"degraded,omitempty"`
}

// Source records what one enumeration source contributed. Every source appears,
// succeeded or not, which makes a silently empty one visible.
type Source struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Found      int    `json:"found"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Partial    bool   `json:"partial,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Stats are the run counters.
type Stats struct {
	Enumerated   int `json:"enumerated"`
	Excluded     int `json:"excluded"`
	InScope      int `json:"in_scope"`
	Live         int `json:"live"`
	Dead         int `json:"dead"`
	Wildcard     int `json:"wildcard"`
	OpenPorts    int `json:"open_ports"`
	HTTPServices int `json:"http_services"`
}

// Host statuses. A host the run never reached is discovered and produces no
// observation: silence is not a measurement.
const (
	StatusDiscovered = "discovered"
	StatusLive       = "live"
	StatusDead       = "dead"
	StatusWildcard   = "wildcard"
)

// Reasons a host ended up dead. nxdomain and no_answer differ: a name that
// exists without an address (an MX-only host) is not a name that does not exist.
const (
	ReasonNXDomain = "nxdomain"
	ReasonNoAnswer = "no_answer"
	ReasonTimeout  = "timeout"
	ReasonWildcard = "wildcard"
)

// Host is one name and everything learned about it.
type Host struct {
	Host      string   `json:"host"`
	Status    string   `json:"status"`
	Addresses []string `json:"addresses,omitempty"`
	CNAME     []string `json:"cname,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	// Sources are the enumeration sources that returned this host. Absent on a
	// verification run, where stage one was a supplied list.
	Sources []string `json:"sources,omitempty"`
	CDN     []CDN    `json:"cdn,omitempty"`
	Ports   []Port   `json:"ports,omitempty"`
	// Scan is what the sweep tried, the only thing letting the tcp layer conclude
	// anything: an empty open-port list means both "nothing else is open" and
	// "nothing else was tried". Absent until the scanner carries it; absence is handled.
	Scan *Scan `json:"scan,omitempty"`
}

// Scan is the port sweep's accounting for one host, summed over every address it
// resolved to.
//
// The four buckets must add up to Scanned, which is not bookkeeping: a probe that
// failed on a local limit (out of file descriptors) says nothing about the target
// and is neither refused nor filtered, and dropping it silently would leave "every
// port refused" true over ports never tried.
type Scan struct {
	Scanned  int `json:"scanned"`
	Open     int `json:"open"`
	Refused  int `json:"refused"`
	Filtered int `json:"filtered"`
	// Unknown is what the observer could not measure, as opposed to what the target did.
	Unknown int `json:"unknown"`
}

// Accounted reports whether the buckets cover the sweep. A report whose counts do
// not add up is unreadable; reading it anyway would conclude a death over ports
// nobody can account for.
func (s Scan) Accounted() bool {
	return s.Scanned > 0 && s.Open+s.Refused+s.Filtered+s.Unknown == s.Scanned
}

// CDN records that some of a host's addresses sit behind an edge.
type CDN struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Addresses   []string `json:"addresses,omitempty"`
	ScanLimited bool     `json:"scan_limited"`
}

// Port is an open port, with the HTTP service behind it if any.
type Port struct {
	Port      int      `json:"port"`
	Protocol  string   `json:"protocol"`
	State     string   `json:"state"`
	Addresses []string `json:"addresses,omitempty"`
	HTTP      *HTTP    `json:"http,omitempty"`
}

// HTTP describes the service answering on a port.
type HTTP struct {
	URL                string   `json:"url"`
	FinalURL           string   `json:"final_url,omitempty"`
	Scheme             string   `json:"scheme"`
	StatusCode         int      `json:"status_code"`
	Title              string   `json:"title,omitempty"`
	ContentLength      int64    `json:"content_length,omitempty"`
	ResponseTimeMS     int64    `json:"response_time_ms,omitempty"`
	Server             string   `json:"server,omitempty"`
	Redirects          []string `json:"redirects,omitempty"`
	RedirectUnfollowed bool     `json:"redirect_unfollowed,omitempty"`
	Tech               []string `json:"tech,omitempty"`
	TLS                *TLS     `json:"tls,omitempty"`
}

// TLS is what the handshake yielded.
type TLS struct {
	SubjectCN    string    `json:"subject_cn,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
	NotAfter     time.Time `json:"not_after,omitzero"`
	SANs         []string  `json:"sans,omitempty"`
	CertSPKIHash string    `json:"cert_spki_hash,omitempty"`
}

// Excluded records a host an exclusion pattern removed before any packet.
type Excluded struct {
	Host    string `json:"host"`
	Pattern string `json:"pattern"`
}

// degradedCodes a run may report. The list grows one code at a time, and a
// consumer not knowing one still sees "degraded", enough to refuse to conclude a
// death.
const (
	DegradedResolversUnvalidated = "resolvers_unvalidated"
	DegradedWildcardZonesCapped  = "wildcard_zones_capped"
)

// RanDegraded reports whether this run may conclude a death. It guards a verdict
// from an observer that could not vouch for itself; truncation is the other
// direction and already respected, as an unreached host produces no observation.
func (r Report) RanDegraded() bool { return len(r.Run.Degraded) > 0 }
