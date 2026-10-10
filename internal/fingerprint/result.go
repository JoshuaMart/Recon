package fingerprint

import "time"

// Result is the rendering service's document. Transcribed, not imported, like the
// scanner's report: the service ships on its own cycle and a shared type would
// make its refactor an outage here. Unknown fields are counted at normalization,
// not refused, since its updates add fields.
type Result struct {
	URL string `json:"url"`
	// Chain is one entry per hop, ordered. The sequence is the information and is
	// never sorted: a reordered chain claims another route.
	Chain         []Hop             `json:"chain"`
	Technologies  []Technology      `json:"technologies"`
	Cookies       map[string]string `json:"cookies"`
	Metadata      Metadata          `json:"metadata"`
	ExternalHosts []string          `json:"external_hosts"`
	WebSockets    []string          `json:"web_sockets"`
	Scripts       []Script          `json:"scripts"`
	Network       Network           `json:"network"`
	// Screenshot is never asked for or stored. Declared so a service sending one anyway
	// is dropped at normalization, not written to a column.
	Screenshot string    `json:"screenshot,omitempty"`
	ScannedAt  time.Time `json:"scanned_at,omitzero"`
	Version    string    `json:"version"`
}

// Hop is one navigation the browser made, including what JavaScript did.
type Hop struct {
	URL             string            `json:"url"`
	StatusCode      int               `json:"status_code"`
	Headers         map[string]string `json:"headers"`
	Title           string            `json:"title"`
	ResponseSize    int64             `json:"response_size"`
	RemoteIPAddress string            `json:"remote_ip_address"`
}

// Technology carries its evidence, making a detection auditable, not asserted.
type Technology struct {
	Name     string         `json:"name"`
	Version  string         `json:"version,omitempty"`
	Category string         `json:"category,omitempty"`
	CPE      string         `json:"cpe,omitempty"`
	Proof    map[string]any `json:"proof,omitempty"`
}

// Script is one script with the hash of its content.
type Script struct {
	URL      string `json:"url"`
	Internal bool   `json:"internal"`
	Hash     string `json:"hash"`
}

// Metadata is what the render found beside the page.
type Metadata struct {
	RobotsTxt bool `json:"robots_txt"`
	LLMsTxt   bool `json:"llms_txt"`
	// Sitemap is where one was found, not whether one exists. Its boolean neighbours
	// made it look like the same kind; it comes back as a URL, the decode failed on the
	// whole document and every render of a host with a sitemap was lost. An undecodable
	// render writes nothing, so the asset stayed as it was and the failure was one log
	// line.
	Sitemap *string `json:"sitemap"`
	// Favicon is the bytes as a data URI, null past the inline size bound. A null here
	// and in FaviconURL differ (too large versus inline), and confusing them concludes
	// a 90 KB icon does not exist.
	Favicon     *string `json:"favicon"`
	FaviconURL  *string `json:"favicon_url"`
	FaviconHash *string `json:"favicon_hash"`
}

// Network is the service's own resolution of the target. Its addresses are dropped
// at normalization: the dns layer resolved them once, and a second producer cannot
// agree with the first on a geo-balanced name.
type Network struct {
	Host string   `json:"host"`
	IPs  []string `json:"ips"`
	// One canonical name, not a chain. The service reports the name it lands on while
	// the scanner reports the chain it walked: same key, different shapes. Read as a
	// list it failed to decode and took the whole render with it.
	CNAME string `json:"cname,omitempty"`
}

// Final is the last hop, the page actually obtained. Its presence is the whole test
// for whether a browser got a page: it decides the outcome and whether
// last_fingerprint_at moves. "Rendered five minutes ago, no cookies" on an asset no
// browser rendered is the false statement the three cookie states prevent.
func (r *Result) Final() (Hop, bool) {
	if r == nil || len(r.Chain) == 0 {
		return Hop{}, false
	}
	return r.Chain[len(r.Chain)-1], true
}
