package fingerprint

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JoshuaMart/recon/internal/canon"
)

// Data is the fingerprint observation: everything a render says about the
// service, and nothing that measures the request.
type Data struct {
	Chain         []HopData    `json:"chain"`
	Technologies  []TechData   `json:"technologies,omitempty"`
	CookieNames   []string     `json:"cookie_names,omitempty"`
	Scripts       []ScriptData `json:"scripts,omitempty"`
	ExternalHosts []string     `json:"external_hosts,omitempty"`
	WebSockets    []string     `json:"web_sockets,omitempty"`
	Robots        bool         `json:"robots,omitempty"`
	LLMsTXT       bool         `json:"llms_txt,omitempty"`
	Sitemap       string       `json:"sitemap,omitempty"`
	FaviconURL    string       `json:"favicon_url,omitempty"`
	FaviconHash   string       `json:"favicon_hash,omitempty"`
	// ExternalRedirect is where the page sent the browser, on another host,
	// without its query: an SSO request carries a fresh state on every render.
	ExternalRedirect string `json:"external_redirect,omitempty"`
}

// HopData is one hop: the chain's order is the information, so it is kept.
type HopData struct {
	URL        string            `json:"url"`
	StatusCode int               `json:"status_code"`
	Title      string            `json:"title,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// TechData is one technology as observed. The proof is kept in asset_tech, not
// compared: the same detection can cite different evidence on two renders.
type TechData struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Category string `json:"category,omitempty"`
}

// ScriptData is one script: its URL and content hash.
type ScriptData struct {
	URL      string `json:"url"`
	Internal bool   `json:"internal,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// droppedHeaders measure the request rather than the service.
var droppedHeaders = map[string]bool{
	"date": true, "age": true, "content-length": true, "expires": true, "connection": true,
	"keep-alive": true, "etag": true, "last-modified": true, "retry-after": true, "set-cookie": true,
}

// volatileMarkers mark a header whose presence is evidence and whose value is
// noise: CF-RAY is a CDN signature and a request id at once.
var volatileMarkers = []string{"-id", "-trace", "-request", "-ray", "-time", "-timing", "runtime", "-duration", "-latency"}

// cacheHeaders say which edge or cache answered this request: HIT on one
// render, MISS on the next, another POP. The name is a CDN signature, the
// value is noise.
var cacheHeaders = map[string]bool{
	"cf-cache-status": true, "x-cache": true, "x-cache-hits": true, "x-cache-status": true,
	"x-served-by": true, "x-amz-cf-pop": true, "x-varnish": true, "via": true,
	"x-proxy-cache": true, "x-cdn-pop": true, "x-edge-location": true, "akamai-cache-status": true,
}

var nonce = regexp.MustCompile(`'nonce-[^']*'`)

func header(name, value string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if droppedHeaders[n] {
		return "", false
	}
	if cacheHeaders[n] {
		return "", true
	}
	for _, m := range volatileMarkers {
		if strings.Contains(n, m) {
			return "", true
		}
	}
	if n == "content-security-policy" || n == "content-security-policy-report-only" {
		// The nonce is regenerated per response; the policy is a fingerprint.
		return nonce.ReplaceAllString(value, "'nonce-'"), true
	}
	return strings.TrimSpace(value), true
}

// Normalize turns a render into its observation.
func Normalize(r *Result) Data {
	d := Data{}
	for _, h := range r.Chain {
		hop := HopData{URL: canonicalURL(h.URL), StatusCode: h.StatusCode}
		if h.Title != nil {
			hop.Title = strings.TrimSpace(*h.Title)
		}
		for k, v := range h.Headers {
			n := strings.ToLower(strings.TrimSpace(k))
			if v, keep := header(n, v); keep {
				if hop.Headers == nil {
					hop.Headers = map[string]string{}
				}
				hop.Headers[n] = v
			}
		}
		d.Chain = append(d.Chain, hop)
	}
	for _, t := range r.Technologies {
		d.Technologies = append(d.Technologies, TechData{Name: strings.TrimSpace(t.Name), Version: strings.TrimSpace(t.Version), Category: t.Category})
	}
	slices.SortFunc(d.Technologies, func(a, b TechData) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	for name := range r.Cookies {
		if n := strings.TrimSpace(name); n != "" && !randomName(n) {
			d.CookieNames = append(d.CookieNames, n)
		}
	}
	slices.Sort(d.CookieNames)
	for _, s := range r.Scripts {
		d.Scripts = append(d.Scripts, ScriptData{URL: canonicalURL(s.URL), Internal: s.Internal, Hash: strings.ToLower(s.Hash)})
	}
	slices.SortFunc(d.Scripts, func(a, b ScriptData) int { return strings.Compare(a.URL+a.Hash, b.URL+b.Hash) })
	d.Scripts = slices.Compact(d.Scripts)
	d.ExternalHosts = hosts(r.ExternalHosts)
	d.WebSockets = sortedLower(r.WebSockets)
	d.ExternalRedirect = canonicalURL(r.ExternalRedirect)
	if m := r.Metadata; m != nil {
		d.Robots, d.LLMsTXT = m.RobotsTXT, m.LLMsTXT
		d.Sitemap = deref(m.Sitemap)
		d.FaviconURL = canonicalURL(deref(m.FaviconURL))
		d.FaviconHash = deref(m.FaviconHash)
	}
	return d
}

// randomName rejects a cookie name generated per session: it links nothing to
// anything, and would defeat deduplication on every render.
func randomName(n string) bool {
	if len(n) < 16 {
		return false
	}
	digits, letters := 0, 0
	for _, c := range n {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letters++
		}
	}
	return digits >= 4 && letters >= 4 && !strings.ContainsAny(n, "_-.")
}

func canonicalURL(u string) string {
	if u == "" {
		return ""
	}
	if c, err := canon.URL(u); err == nil {
		return c
	}
	return u
}

func hosts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		if n, err := canon.FQDN(h); err == nil {
			out = append(out, n)
		} else if p, err := url.Parse(h); err == nil && p.Hostname() != "" {
			out = append(out, strings.ToLower(p.Hostname()))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func sortedLower(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// versionPattern is the leading dotted number of a version string.
var versionPattern = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,5})`)

// ParseVersion returns a version's numeric components, which PostgreSQL
// compares element by element, or nil when there are none. 1.24 stays {1,24}:
// padding it would invent precision.
func ParseVersion(raw string) []int32 {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(strings.ToLower(raw)))
	if m == nil {
		return nil
	}
	parts := strings.Split(m[1], ".")
	out := make([]int32, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			return nil
		}
		out = append(out, int32(n))
	}
	return out
}

// Precision reports whether two versions differ only in precision: one is a
// prefix of the other, 1.24 against 1.24.0.
func Precision(a, b string) bool {
	pa, pb := ParseVersion(a), ParseVersion(b)
	if pa == nil || pb == nil || len(pa) == len(pb) {
		return false
	}
	short, long := pa, pb
	if len(pa) > len(pb) {
		short, long = pb, pa
	}
	return slices.Equal(short, long[:len(short)])
}
