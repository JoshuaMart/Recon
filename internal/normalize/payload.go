package normalize

import (
	"regexp"
	"sort"
	"strings"
)

// Layer is what an observation describes. One producer per layer, so a layer has
// one chain and one normalization.
type Layer string

const (
	LayerDNS         Layer = "dns"
	LayerTCP         Layer = "tcp"
	LayerHTTP        Layer = "http"
	LayerFingerprint Layer = "fingerprint"
)

// Result is a normalized payload and what the schema declaration noticed.
//
// Unknown fields are accepted and counted, not rejected: the rendering service
// ships on its own cycle and adds fields, so strict rejection would turn every
// release into an ingestion outage. The counter is an alerted metric, so a
// `techs` emitted instead of `technologies` shows up by name immediately.
type Result struct {
	Data    map[string]any
	Unknown []string
}

// Payload applies the single normalization every write goes through. The
// comparison deduplication performs only means something on normalized
// structures; otherwise it deduplicates nothing and raises no error.
func Payload(layer Layer, data map[string]any) (Result, error) {
	def, ok := schemas[layer]
	if !ok {
		return Result{}, invalid("unknown layer %q", layer)
	}

	out := make(map[string]any, len(data))
	for key, value := range data {
		out[key] = value
	}

	for _, name := range def.required {
		if _, present := out[name]; !present {
			return Result{}, invalid("layer %s requires %q", layer, name)
		}
	}

	var unknown []string
	for key := range out {
		if !def.declares(key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)

	def.apply(out)
	return Result{Data: out, Unknown: unknown}, nil
}

type schema struct {
	required []string
	optional []string
	apply    func(map[string]any)
}

func (s schema) declares(name string) bool {
	for _, n := range s.required {
		if n == name {
			return true
		}
	}
	for _, n := range s.optional {
		if n == name {
			return true
		}
	}
	return false
}

var schemas = map[Layer]schema{
	LayerDNS: {
		required: []string{"status"},
		// No TTL anywhere, on purpose: a TTL decreases between passes, so keeping one
		// would make every DNS observation differ and zero this layer's deduplication.
		optional: []string{"reason", "addresses", "cname", "takeover_candidate"},
		apply: func(d map[string]any) {
			// Addresses are a set: the same answers in another order are the same answer.
			sortStrings(d, "addresses")
			// The CNAME chain is ordered (name to target); sorting would claim another chain.
		},
	},

	LayerTCP: {
		required: []string{"open_ports"},
		// scan is what the sweep tried, as counts, and the only thing that lets this
		// layer conclude anything: an empty open-port list means both "nothing else is
		// open" and "nothing else was tried", which decide between death and silence.
		optional: []string{"addresses", "cdn", "scan"},
		apply: func(d map[string]any) {
			sortNumbers(d, "open_ports")
			sortStrings(d, "addresses")
		},
	},

	LayerHTTP: {
		required: []string{"scheme", "status_code"},
		optional: []string{
			"url", "final_url", "title", "server", "redirects",
			"tech", "tls", "waf_detected", "waf_vendor", "waf_source",
			// Derived at ingestion, not reported. They are in the payload as well as the
			// projection so the journal stays readable alone: an asset fronted last month
			// and not today is a change worth seeing.
			"is_cdn", "cdn_provider", "origin_dead", "takeover_candidate",
			// Both measure the request that just happened. A page with a CSRF token or
			// session id differs every pass, in the busiest layer.
			"response_time_ms", "content_length",
		},
		apply: func(d map[string]any) {
			delete(d, "response_time_ms")
			delete(d, "content_length")
			// The redirect sequence is the information; sorting would say another route.
			sortStrings(d, "tech")
			if tls, ok := d["tls"].(map[string]any); ok {
				sortStrings(tls, "sans")
			}
		},
	},

	LayerFingerprint: {
		required: []string{"url"},
		optional: []string{
			"chain", "technologies", "cookies", "cookie_names", "metadata", "external_hosts",
			"web_sockets", "scripts", "network", "screenshot", "scanned_at", "version",
		},
		apply: func(d map[string]any) {
			// The capture never reaches the database. Two barriers exist upstream; this one
			// makes it impossible rather than instructed, as every write comes through here.
			delete(d, "screenshot")
			// observation.observed_at already carries it and it is the partition key; in the
			// payload it only duplicates.
			delete(d, "scanned_at")
			// Promoted to observation.producer_version, which deduplication does not compare.
			// Leaving it here would smuggle it back in, and every version bump would rewrite
			// the whole inventory's payload.
			delete(d, "version")

			// Addresses belong to the DNS layer, which resolved them once. A renderer
			// resolving the same name is a second producer of a value it does not own, and
			// they disagree on a geo-balanced or fronted name.
			if network, ok := d["network"].(map[string]any); ok {
				delete(network, "ips")
			}

			if chain, ok := d["chain"].([]any); ok {
				for _, hop := range chain {
					hop, ok := hop.(map[string]any)
					if !ok {
						continue
					}
					delete(hop, "remote_ip_address")
					if headers, ok := hop["headers"].(map[string]any); ok {
						normalizeHeaders(headers)
					}
				}
			}

			normalizeCookies(d)
			sortStrings(d, "external_hosts")
			sortStrings(d, "web_sockets")
		},
	},
}

// normalizeCookies keeps the names and erases the values. A name identifies an
// application and is a pivot; a value is a session identifier reissued each scan.
// The loss is deliberate: nothing reliably tells a stable application cookie from
// a session one, and guessing wrong costs the layer's deduplication.
func normalizeCookies(d map[string]any) {
	cookies, ok := d["cookies"].(map[string]any)
	if !ok {
		return
	}

	names := make([]string, 0, len(cookies))
	for name := range cookies {
		// A per-session generated name is rejected here, not at display, for a reason a
		// denylist lacks: it links no asset to any other and makes the payload differ
		// every pass. PHPSESSID is useless because it is everywhere, a random name
		// because it is nowhere twice.
		if looksRandom(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	delete(d, "cookies")
	if len(names) > 0 {
		out := make([]any, len(names))
		for i, name := range names {
			out[i] = name
		}
		d["cookie_names"] = out
	}
}

// hopChainHeaders read from the first relay to the last: a sequence, not a set.
var hopChainHeaders = map[string]bool{
	"via": true, "forwarded": true, "x-forwarded-for": true,
	"x-forwarded-host": true, "x-forwarded-proto": true,
	"server-timing": true, "trailer": true,
}

// removedHeaders measure the request, not the service.
var removedHeaders = map[string]bool{
	"date": true, "age": true, "content-length": true, "expires": true,
	"connection": true, "keep-alive": true, "etag": true,
	"last-modified": true, "retry-after": true,
}

// emptiedExceptions are names the generic rule would empty that carry something
// worth keeping.
var emptiedExceptions = map[string]bool{"x-requested-with": true}

var (
	// Any header whose name says it carries an identifier or a duration. The generic
	// rule covers what nobody listed yet: a suffix match once missed
	// x-response-time-ms, and one chain grew every pass for that alone.
	volatileHeader = regexp.MustCompile(`-(id|trace|request|ray)\b|-(time|timing|duration|latency|elapsed)|runtime`)
	// Regenerated on every response, inside a header that is otherwise among the
	// most informative fingerprints of a page.
	cspNonce = regexp.MustCompile(`'nonce-[^']*'`)
)

func normalizeHeaders(headers map[string]any) {
	for name, value := range headers {
		lower := strings.ToLower(name)

		if removedHeaders[lower] {
			delete(headers, name)
			continue
		}

		// The presence is evidence, the value noise. CF-RAY is a CDN signature and a
		// request id at once; dropping it would lose the signal.
		if volatileHeader.MatchString(lower) && !emptiedExceptions[lower] {
			headers[name] = ""
			continue
		}

		text, ok := value.(string)
		if !ok {
			continue
		}
		if lower == "content-security-policy" || lower == "content-security-policy-report-only" {
			// Replaced, not removed: script-src 'self' 'nonce-' and script-src 'self' are
			// two different policies.
			headers[name] = cspNonce.ReplaceAllString(text, "'nonce-'")
			continue
		}
		if !hopChainHeaders[lower] && strings.Contains(text, ",") {
			headers[name] = sortCommaList(text)
		}
	}
}

// looksRandom reports whether a cookie name was generated rather than chosen. Conservative,
// as the errors cost differently: a false positive silently drops a real pivot, a false
// negative leaves one useless name in an index. So it needs every sign at once: no separator,
// digits mixed into the letters, too few vowels to pronounce. PHPSESSID and JSESSIONID (no
// digit) and wordpress_logged_in_9f (a separator) are deliberately kept; the display denylist
// handles them, removing a badge and never data.
func looksRandom(name string) bool {
	if len(name) < 8 || len(name) > 64 {
		return false
	}
	if strings.ContainsAny(name, "-_.") {
		return false
	}

	var digits, letters, vowels, lastLetter, firstDigit int
	firstDigit = -1
	lastLetter = -1

	for i, c := range strings.ToLower(name) {
		switch {
		case c >= '0' && c <= '9':
			digits++
			if firstDigit < 0 {
				firstDigit = i
			}
		case c >= 'a' && c <= 'z':
			letters++
			lastLetter = i
			if strings.ContainsRune("aeiouy", c) {
				vowels++
			}
		default:
			// Anything else is a name someone typed.
			return false
		}
	}

	if letters == 0 || digits < 3 {
		return false
	}
	// A version suffix is not a random name; digits sitting inside the word mark one.
	if firstDigit > lastLetter {
		return false
	}
	// A pronounceable name is a chosen one.
	return float64(vowels) <= float64(letters)*0.25
}

func sortStrings(d map[string]any, name string) {
	list, ok := d[name].([]any)
	if !ok {
		return
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return
		}
		out = append(out, text)
	}
	sort.Strings(out)

	back := make([]any, len(out))
	for i, text := range out {
		back[i] = text
	}
	d[name] = back
}

func sortNumbers(d map[string]any, name string) {
	list, ok := d[name].([]any)
	if !ok {
		return
	}
	out := make([]float64, 0, len(list))
	for _, item := range list {
		n, ok := toFloat(item)
		if !ok {
			return
		}
		out = append(out, n)
	}
	sort.Float64s(out)

	back := make([]any, len(out))
	for i, n := range out {
		back[i] = n
	}
	d[name] = back
}

func sortCommaList(value string) string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func toFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
