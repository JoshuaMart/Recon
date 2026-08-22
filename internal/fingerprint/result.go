package fingerprint

import "time"

// Result is the rendering service's document.
//
// Transcribed rather than imported, for the same reason the scanner's report
// is: the service ships on its own cycle, and a shared type would make its
// refactor an outage here. Unknown fields are counted at normalization rather
// than refused, because its updates add fields and every release would
// otherwise become an ingestion outage.
type Result struct {
	URL string `json:"url"`
	// Chain is one entry per hop, ordered. The sequence is the information, so
	// it is never sorted: a reordered chain claims the request took another
	// route.
	Chain         []Hop             `json:"chain"`
	Technologies  []Technology      `json:"technologies"`
	Cookies       map[string]string `json:"cookies"`
	Metadata      Metadata          `json:"metadata"`
	ExternalHosts []string          `json:"external_hosts"`
	WebSockets    []string          `json:"web_sockets"`
	Scripts       []Script          `json:"scripts"`
	Network       Network           `json:"network"`
	// Screenshot is never asked for and never stored. It is declared so that a
	// service that sends one anyway is dropped at normalization rather than
	// written to a column.
	Screenshot string    `json:"screenshot,omitempty"`
	ScannedAt  time.Time `json:"scanned_at,omitzero"`
	Version    string    `json:"version"`
}

// Hop is one navigation the browser actually made, including what JavaScript
// did.
type Hop struct {
	URL             string            `json:"url"`
	StatusCode      int               `json:"status_code"`
	Headers         map[string]string `json:"headers"`
	Title           string            `json:"title"`
	ResponseSize    int64             `json:"response_size"`
	RemoteIPAddress string            `json:"remote_ip_address"`
}

// Technology carries its evidence, which is what makes a detection auditable
// rather than an assertion.
type Technology struct {
	Name     string         `json:"name"`
	Version  string         `json:"version,omitempty"`
	Category string         `json:"category,omitempty"`
	CPE      string         `json:"cpe,omitempty"`
	Proof    map[string]any `json:"proof,omitempty"`
}

// Script is one script with the hash of its content, per script.
type Script struct {
	URL      string `json:"url"`
	Internal bool   `json:"internal"`
	Hash     string `json:"hash"`
}

// Metadata is what the render found beside the page.
type Metadata struct {
	RobotsTxt bool `json:"robots_txt"`
	LLMsTxt   bool `json:"llms_txt"`
	// Sitemap is where one was found, not whether one exists. The two
	// neighbours above are booleans and this one is not, which is what made it
	// look like a third of the same kind: it came back as a URL, the decode
	// failed on the whole document, and every render of a host that publishes a
	// sitemap was lost with it. A render that cannot be decoded writes nothing
	// at all, so the asset stayed exactly as it was and the failure was a line
	// in a log.
	Sitemap *string `json:"sitemap"`
	// Favicon is the bytes as a data URI, null past the inline size bound. A
	// null here and a null in FaviconURL do not mean the same thing: the first
	// says the icon was too large, the second that it was inline, and a
	// consumer confusing them concludes that a 90 KB icon does not exist.
	Favicon     *string `json:"favicon"`
	FaviconURL  *string `json:"favicon_url"`
	FaviconHash *string `json:"favicon_hash"`
}

// Network is the service's own resolution of the target. Its addresses are
// dropped at normalization: the dns layer resolved them once, and a second
// producer for a value it does not own cannot agree with the first on a geo
// balanced name.
type Network struct {
	Host string   `json:"host"`
	IPs  []string `json:"ips"`
	// One canonical name and not a chain. The service resolves the target host
	// and reports the name it lands on, where the scanner reports the chain it
	// walked, and the two carry the same key with different shapes. Read as a
	// list it failed to decode, and it took the whole render with it.
	CNAME string `json:"cname,omitempty"`
}

// Final is the last hop, which is the page that was actually obtained.
//
// Its presence is the whole test for whether a browser got a page. It decides
// the outcome, and it decides whether last_fingerprint_at moves: a list showing
// "rendered five minutes ago, no cookies" on an asset no browser ever rendered
// is the false statement the three cookie states exist to prevent.
func (r *Result) Final() (Hop, bool) {
	if r == nil || len(r.Chain) == 0 {
		return Hop{}, false
	}
	return r.Chain[len(r.Chain)-1], true
}
