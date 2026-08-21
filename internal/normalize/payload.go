package normalize

import (
	"regexp"
	"sort"
	"strings"
)

// Layer is what an observation describes. One producer per layer, which is
// what lets a layer have one chain and one normalization.
type Layer string

const (
	LayerDNS         Layer = "dns"
	LayerTCP         Layer = "tcp"
	LayerHTTP        Layer = "http"
	LayerFingerprint Layer = "fingerprint"
)

// Result is a normalized payload and what the schema declaration noticed.
//
// Unknown fields are accepted and counted rather than rejected. Strict
// rejection is too rigid: the rendering service ships on its own cycle and its
// updates add fields, so every release would become an ingestion outage. The
// counter is the compromise, exposed as an alerted metric rather than a log
// line, so a `techs` emitted instead of `technologies` shows up by name and
// immediately.
type Result struct {
	Data    map[string]any
	Unknown []string
}

// Payload applies the single normalization every write goes through.
//
// The comparison deduplication performs only means something on normalized
// structures. Without this, deduplication deduplicates nothing, the volume
// stays at its raw order of magnitude, and no error is raised anywhere.
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
		// No TTL anywhere, and that absence is load-bearing rather than
		// incidental: a TTL decreases between two passes by construction, so
		// keeping one would make every DNS observation differ from the last
		// and take this layer's deduplication to zero.
		optional: []string{"reason", "addresses", "cname", "takeover_candidate"},
		apply: func(d map[string]any) {
			// Addresses are a set: two answers listing the same ones in a
			// different order are the same answer.
			sortStrings(d, "addresses")
			// The CNAME chain is not. It reads from the name to its target,
			// and sorting it would claim a different chain.
		},
	},

	LayerTCP: {
		required: []string{"open_ports"},
		optional: []string{"addresses", "cdn", "scanned_ports", "closed_ports", "filtered_ports"},
		apply: func(d map[string]any) {
			sortNumbers(d, "open_ports")
			sortStrings(d, "addresses")
			// A hundred identical numbers on every asset are the probe's
			// settings copied once per row. Their counts are another matter:
			// "one open out of a hundred scanned" separates "nothing else is
			// open" from "nothing else was tried".
			for _, name := range []string{"scanned_ports", "closed_ports", "filtered_ports"} {
				if list, ok := d[name]; ok {
					d[name+"_count"] = length(list)
					delete(d, name)
				}
			}
		},
	},

	LayerHTTP: {
		required: []string{"scheme", "status_code"},
		optional: []string{
			"url", "final_url", "title", "server", "redirects",
			"tech", "tls", "waf_detected", "waf_vendor",
			// Both measure the request that just happened. A page carrying a
			// CSRF token or a session id differs on every pass, and this is
			// the busiest layer of the system.
			"response_time_ms", "content_length",
		},
		apply: func(d map[string]any) {
			delete(d, "response_time_ms")
			delete(d, "content_length")
			// The redirect sequence is the information. Sorting it would say
			// the request took another route.
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
			// The capture never reaches the database. Two barriers exist
			// upstream; this is the one that makes it an impossibility rather
			// than an instruction, because every write comes through here.
			delete(d, "screenshot")
			// observation.observed_at already carries this, and it is the
			// partition key. Keeping it in the payload duplicates it in the
			// one place where it costs something.
			delete(d, "scanned_at")
			// Promoted to observation.producer_version, a column deduplication
			// deliberately does not compare. Leaving it here would smuggle it
			// back into the comparison, and every version bump would rewrite
			// the payload of the whole inventory at once.
			delete(d, "version")

			// Addresses belong to the DNS layer, which resolved them once. A
			// renderer resolving the same name is a second producer for a
			// value it does not own, and the two cannot agree on a geo
			// balanced or fronted name.
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

// normalizeCookies keeps the names and erases the values.
//
// A name identifies an application and is a pivot; a value is a session
// identifier reissued on every scan. This is a deliberate loss: a diff on a
// non-volatile application cookie would have meaning, but nothing reliably
// tells such a cookie from a session one, and being wrong in that direction
// costs the whole layer's deduplication.
func normalizeCookies(d map[string]any) {
	cookies, ok := d["cookies"].(map[string]any)
	if !ok {
		return
	}

	names := make([]string, 0, len(cookies))
	for name := range cookies {
		// A name generated per session is rejected here rather than at
		// display, and for a reason a denylist does not have: it cannot link
		// any asset to any other, and it makes the payload differ on every
		// pass. PHPSESSID is useless because it is everywhere, a random name
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

// hopChainHeaders read from the first relay to the last, so they are a
// sequence rather than a set. Sorting them would claim another route.
var hopChainHeaders = map[string]bool{
	"via": true, "forwarded": true, "x-forwarded-for": true,
	"x-forwarded-host": true, "x-forwarded-proto": true,
	"server-timing": true, "trailer": true,
}

// removedHeaders measure the request rather than the service.
var removedHeaders = map[string]bool{
	"date": true, "age": true, "content-length": true, "expires": true,
	"connection": true, "keep-alive": true, "etag": true,
	"last-modified": true, "retry-after": true,
}

// emptiedExceptions are the names the generic rule below would empty and that
// carry something worth keeping.
var emptiedExceptions = map[string]bool{"x-requested-with": true}

var (
	// Any header whose name says it carries an identifier or a duration. The
	// generic rule covers what nobody has listed yet, which is the point: a
	// suffix match once missed x-response-time-ms and one chain grew on every
	// pass for that alone.
	volatileHeader = regexp.MustCompile(`-(id|trace|request|ray)\b|-(time|timing|duration|latency|elapsed)|runtime`)
	// Regenerated on every response, inside a header that is otherwise one of
	// the most informative fingerprints of a page.
	cspNonce = regexp.MustCompile(`'nonce-[^']*'`)
)

func normalizeHeaders(headers map[string]any) {
	for name, value := range headers {
		lower := strings.ToLower(name)

		if removedHeaders[lower] {
			delete(headers, name)
			continue
		}

		// The presence is evidence and the value is noise. CF-RAY is a CDN
		// signature and a request id at once, so dropping it to be rid of the
		// value would throw away the signal with it.
		if volatileHeader.MatchString(lower) && !emptiedExceptions[lower] {
			headers[name] = ""
			continue
		}

		text, ok := value.(string)
		if !ok {
			continue
		}
		if lower == "content-security-policy" || lower == "content-security-policy-report-only" {
			// Replaced rather than removed: script-src 'self' 'nonce-' and
			// script-src 'self' are two different policies.
			headers[name] = cspNonce.ReplaceAllString(text, "'nonce-'")
			continue
		}
		if !hopChainHeaders[lower] && strings.Contains(text, ",") {
			headers[name] = sortCommaList(text)
		}
	}
}

// looksRandom reports whether a cookie name was generated rather than chosen.
//
// The test is deliberately conservative, because the two errors do not cost the
// same. A false positive drops a real pivot in silence; a false negative leaves
// one useless name in an index nobody reads. So it takes every sign at once: no
// separator, digits mixed into the letters rather than appended, and too few
// vowels for anybody to pronounce it.
//
// What that keeps, on purpose: PHPSESSID and JSESSIONID carry no digit, and
// wordpress_logged_in_9f carries a separator. Those are handled by the display
// denylist, which removes a badge and never a piece of data.
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
			// Anything else is a name somebody typed.
			return false
		}
	}

	if letters == 0 || digits < 3 {
		return false
	}
	// A version suffix is not a random name. What marks one is digits sitting
	// inside the word rather than after it.
	if firstDigit > lastLetter {
		return false
	}
	// A name people can pronounce is a name somebody chose.
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

func length(value any) int {
	list, ok := value.([]any)
	if !ok {
		return 0
	}
	return len(list)
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
