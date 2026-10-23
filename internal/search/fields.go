package search

import "sort"

// Kind is how a field's values are read and compiled.
type Kind string

// Field kinds.
const (
	KText     Kind = "text"     // equals case-insensitively, glob with *, /regex/
	KDomain   Kind = "domain"   // a name and everything under it, on label boundaries
	KEnum     Kind = "enum"     // one of a fixed set
	KNumber   Kind = "number"   // comparisons, ranges, lists
	KStatus   Kind = "status"   // a number, or a class: 2xx
	KIP       Kind = "ip"       // an address or a network
	KArray    Kind = "array"    // any element matches, as text
	KDate     Kind = "date"     // a date or an age
	KTech     Kind = "tech"     // a technology and its version
	KCategory Kind = "category" // a technology category
	KHeader   Kind = "header"   // header:<name> present, header.<name>:<value>
)

// Field is one field of the language.
type Field struct {
	Name    string   `json:"name"`
	Kind    Kind     `json:"kind"`
	Values  []string `json:"values,omitempty"` // for an enum
	Doc     string   `json:"doc"`
	Example string   `json:"example"`
	column  string
}

// fields is the registry: what the language accepts, and how each field is
// compiled. GET /api/search/fields serves it.
var fields = map[string]Field{}

func register(f Field) { fields[f.Name] = f }

func init() {
	for _, f := range []Field{
		{Name: "host", Kind: KText, column: "ac.host", Doc: "the host name or address", Example: "host:api.*"},
		{Name: "apex", Kind: KDomain, column: "ac.host", Doc: "a domain and everything under it", Example: "apex:target.com"},
		{Name: "program", Kind: KText, column: "p.name", Doc: "the program's name, or its slug", Example: "program:acme*"},
		{Name: "platform", Kind: KText, column: "p.platform", Doc: "the program's platform", Example: "platform:hackerone"},
		{Name: "kind", Kind: KEnum, column: "ac.kind", Values: []string{"fqdn", "ip", "service", "url"}, Doc: "what the asset is", Example: "kind:service"},
		{Name: "scope", Kind: KEnum, column: "ac.scope_status", Values: []string{"in", "out", "unknown"}, Doc: "the scope status", Example: "scope:unknown"},
		{Name: "state", Kind: KEnum, column: "ac.lifecycle", Values: []string{"candidate", "active", "flapping", "inactive", "unobservable", "archived"}, Doc: "the lifecycle", Example: "state:active,flapping"},
		{Name: "port", Kind: KNumber, column: "ac.port", Doc: "a service's port", Example: "port:8080,8443"},
		{Name: "scheme", Kind: KText, column: "ac.scheme", Doc: "http or https", Example: "scheme:http"},
		{Name: "ip", Kind: KIP, column: "ac.ips", Doc: "an address or a network the asset resolves to", Example: "ip:10.0.0.0/8"},
		{Name: "asn", Kind: KNumber, column: "ac.asn", Doc: "the AS of the first address", Example: "asn:13335"},
		{Name: "asn_org", Kind: KText, column: "ac.asn_org", Doc: "the AS organization", Example: "asn_org:*amazon*"},
		{Name: "country", Kind: KText, column: "ac.country", Doc: "the country of the first address", Example: "country:fr"},
		{Name: "cname", Kind: KArray, column: "ac.cname", Doc: "any name in the CNAME chain", Example: "cname:*.azurewebsites.net"},
		{Name: "cdn", Kind: KText, column: "ac.cdn", Doc: "the CDN FastRecon saw, * for any", Example: "cdn:cloudflare"},
		{Name: "waf", Kind: KText, column: "ac.waf", Doc: "a WAF the Fingerprinter detected, * for any", Example: "waf:*"},
		{Name: "status", Kind: KStatus, column: "ac.status_code", Doc: "the HTTP status, or a class", Example: "status:2xx"},
		{Name: "title", Kind: KText, column: "ac.title", Doc: "the page title", Example: `title:*login*`},
		{Name: "server", Kind: KText, column: "ac.server", Doc: "the Server header FastRecon saw", Example: "server:nginx*"},
		{Name: "final_url", Kind: KText, column: "ac.final_url", Doc: "where the redirects landed", Example: "final_url:*/login*"},
		{Name: "redirect", Kind: KArray, column: "ac.redirect_hosts", Doc: "any host of the browser's redirect chain", Example: "redirect:*.okta.com"},
		{Name: "tech", Kind: KTech, Doc: "a technology, with an optional version after @", Example: "tech:nginx@<1.25"},
		{Name: "category", Kind: KCategory, Doc: "a technology category", Example: `category:"Web servers"`},
		{Name: "header", Kind: KHeader, column: "ac.header_names", Doc: "a header present; header.<name>:<value> for its value", Example: "header.x-powered-by:PHP*"},
		{Name: "cookie", Kind: KArray, column: "ac.cookie_names", Doc: "a cookie name", Example: "cookie:JSESSIONID"},
		{Name: "script", Kind: KArray, column: "ac.script_hashes", Doc: "a script's SHA-256", Example: "script:9f86d0*"},
		{Name: "favicon", Kind: KText, column: "ac.favicon_hash", Doc: "the favicon's mmh3 hash", Example: "favicon:-1293034"},
		{Name: "external", Kind: KArray, column: "ac.external_hosts", Doc: "a third party host the page contacted", Example: "external:*.intercom.io"},
		{Name: "websocket", Kind: KArray, column: "ac.web_sockets", Doc: "a WebSocket URL the page opened", Example: "websocket:wss://*"},
		{Name: "source", Kind: KEnum, column: "a.discovery_source", Values: []string{"fastrecon", "certstream", "bbot", "scopesextractor", "manual"}, Doc: "how the asset first appeared", Example: "source:certstream"},
		{Name: "first_seen", Kind: KDate, column: "ac.first_seen", Doc: "a date, or an age: <7d is less than seven days ago", Example: "first_seen:<24h"},
		{Name: "last_seen", Kind: KDate, column: "ac.last_seen", Doc: "a date or an age", Example: "last_seen:>30d"},
		{Name: "last_changed", Kind: KDate, column: "ac.last_changed", Doc: "a date or an age", Example: "last_changed:<24h"},
		{Name: "triage", Kind: KEnum, column: "COALESCE(tr.state, 'new')", Values: []string{"new", "seen", "interesting", "ignored"}, Doc: "your state on the asset", Example: "triage:interesting"},
		{Name: "tag", Kind: KArray, column: "tr.tags", Doc: "one of your tags", Example: "tag:todo"},
	} {
		register(f)
	}
}

// hasFields are what has: accepts, each a condition that the asset carries
// the thing.
var hasFields = map[string]string{
	"screenshot": "EXISTS (SELECT 1 FROM screenshot s WHERE s.asset_id = ac.asset_id)",
	"favicon":    "ac.favicon_hash IS NOT NULL",
	"robots":     "ac.has_robots",
	"sitemap":    "ac.has_sitemap",
	"llms_txt":   "ac.has_llms_txt",
	"tech":       "EXISTS (SELECT 1 FROM asset_tech t WHERE t.asset_id = ac.asset_id)",
	"websocket":  "cardinality(ac.web_sockets) > 0",
	"cname":      "cardinality(ac.cname) > 0",
	"note":       "tr.note IS NOT NULL AND tr.note <> ''",
	"title":      "ac.title IS NOT NULL AND ac.title <> ''",
	"cdn":        "ac.cdn IS NOT NULL",
	"waf":        "ac.waf IS NOT NULL",
}

// Fields lists the registry, sorted, with what has: accepts.
func Fields() ([]Field, []string) {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	has := make([]string, 0, len(hasFields))
	for h := range hasFields {
		has = append(has, h)
	}
	sort.Strings(has)
	return out, has
}

// suggest is the registered name closest to an unknown one.
func suggest(name string, names []string) string {
	best, bestD := "", 4
	for _, n := range names {
		if d := distance(name, n); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// distance is the Levenshtein distance.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
