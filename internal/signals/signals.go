// Package signals reads what a response means, not what it contains. Three questions are
// answered only here: is this asset fronted, is this response a mitigation, is it an edge
// reporting its origin gone. All are derived at ingestion from the payload, never believed
// from the scanner, and bounded by what a report carries (a raw probe gives status, server
// header, title and technologies; body-based recognition comes with the fingerprint layer).
package signals

import (
	"strings"
)

// Response is the part of an HTTP observation a signature reads.
type Response struct {
	StatusCode int
	Server     string
	Title      string
	Tech       []string
	// Fronted is what the scanner determined about the address before connecting:
	// structural, cheap evidence, not a verdict. It only decides which signatures are
	// worth applying.
	Fronted  bool
	Provider string
}

// Verdict is what one response is worth.
type Verdict struct {
	// Dead names the origin error signature, empty if none. It is an informative
	// failure on the http layer: the probe reached the edge, which reported the origin
	// gone.
	Dead string
	// Challenge names the mitigation signature, empty if none. The target is alive,
	// so it must never drift toward death, and the probe measured nothing, so it is
	// not a success either.
	Challenge string
	Vendor    string
	// Unclaimed names a service answering that nobody owns this name: the half of a
	// takeover candidate DNS cannot see.
	Unclaimed string
}

// Usable reports whether the observer got something out of this response.
// Orthogonal to the outcome: a 403 with a mitigation signature is a target that
// answered and is there, and a probe that learned nothing, driving different
// columns.
func (v Verdict) Usable() bool { return v.Challenge == "" && v.Dead == "" }

// Read applies every signature to one response.
func Read(r Response) Verdict {
	var v Verdict

	server := strings.ToLower(r.Server)
	title := strings.ToLower(r.Title)

	switch {
	// Cloudflare answers 530 for the whole 100x family, with the number in the page.
	// 1016 is an origin DNS error (a takeover candidate as much as a death); 1001 is a
	// resolution error.
	case r.StatusCode == 530 && (strings.Contains(server, "cloudflare") || strings.Contains(title, "cloudflare")):
		v.Dead = "cloudflare_origin_dns"
		v.Vendor = "cloudflare"
		if strings.Contains(title, "1016") || strings.Contains(title, "origin dns") {
			v.Dead = "cloudflare_1016"
			v.Unclaimed = "cloudflare_1016"
		}

	// The edge is up and the origin is not answering. 521, 522 and 523 are a refused
	// connection, a timeout and an unreachable origin.
	case (r.StatusCode == 521 || r.StatusCode == 522 || r.StatusCode == 523) &&
		strings.Contains(server, "cloudflare"):
		v.Dead = "cloudflare_origin_down"
		v.Vendor = "cloudflare"

	// Akamai reports a dead origin as an error page with a reference number. The
	// status alone would match every missing path on the internet, so the server
	// header makes it a signature.
	case strings.Contains(server, "akamaighost") &&
		(strings.Contains(title, "reference #") || strings.Contains(title, "an error occurred")):
		v.Dead = "akamai_reference"
		v.Vendor = "akamai"

	// Nobody has claimed the bucket. The service answers and the name resolves, so
	// nothing at transport level says this.
	case r.StatusCode == 404 && strings.Contains(server, "amazons3"):
		v.Unclaimed = "s3"
		v.Dead = "s3_no_such_bucket"

	case r.StatusCode == 404 && strings.Contains(server, "github.com"):
		v.Unclaimed = "github_pages"
		v.Dead = "github_pages_unclaimed"
	}

	if challenge, vendor := mitigation(r, server, title); challenge != "" {
		v.Challenge = challenge
		if v.Vendor == "" {
			v.Vendor = vendor
		}
	}

	return v
}

// mitigation recognizes a response that is the mitigation itself. A 403 on a
// protected route is a live, measured application (a success); only a 403 with a
// mitigation signature is a challenge. Conflating them is certain error one way
// or the other, costly on about a tenth of a real perimeter.
func mitigation(r Response, server, title string) (string, string) {
	switch {
	case strings.Contains(title, "just a moment"),
		strings.Contains(title, "attention required") && strings.Contains(title, "cloudflare"),
		strings.Contains(title, "checking your browser"):
		return "interstitial", "cloudflare"

	case strings.Contains(title, "request rejected"):
		return "block_page", "imperva"

	case r.StatusCode == 403 && strings.Contains(server, "akamaighost") &&
		strings.Contains(title, "access denied"):
		return "block_page", "akamai"

	case r.StatusCode == 406 && strings.Contains(server, "imperva"):
		return "block_page", "imperva"

	// A rate limit is the target telling the observer to stop: alive, and nothing
	// measured about the service, like a challenge.
	case r.StatusCode == 429:
		return "rate_limited", vendorOf(r, server)
	}

	// A WAF-category technology only means "a WAF is here". Cloudflare is reported on
	// every response it fronts, even a normal 200, so reading it as a challenge would
	// mark every legitimate 403 of a fronted application unmeasurable.
	return "", ""
}

func vendorOf(r Response, server string) string {
	if r.Provider != "" {
		return r.Provider
	}
	for _, known := range []string{"cloudflare", "akamai", "fastly", "imperva"} {
		if strings.Contains(server, known) {
			return known
		}
	}
	return ""
}

// cdnCNAMESuffixes are terminal names saying an asset is fronted even when address
// enrichment says nothing.
var cdnCNAMESuffixes = map[string]string{
	".edgekey.net":           "akamai",
	".edgesuite.net":         "akamai",
	".akamaiedge.net":        "akamai",
	".cloudfront.net":        "cloudfront",
	".fastly.net":            "fastly",
	".fastlylb.net":          "fastly",
	".cdn.cloudflare.net":    "cloudflare",
	".azureedge.net":         "azure",
	".azurefd.net":           "azure",
	".stackpathdns.com":      "stackpath",
	".impervadns.net":        "imperva",
	".b-cdn.net":             "bunny",
	".gcdn.co":               "gcore",
	".global.ssl.fastly.net": "fastly",
}

// cdnOperators are operator names meaning the same as a terminal CNAME when the
// address was enriched but the name is direct.
var cdnOperators = []struct{ needle, provider string }{
	{"cloudflare", "cloudflare"},
	{"akamai", "akamai"},
	{"fastly", "fastly"},
	{"amazon cloudfront", "cloudfront"},
	{"incapsula", "imperva"},
	{"imperva", "imperva"},
	{"stackpath", "stackpath"},
	{"bunny", "bunny"},
	{"gcore", "gcore"},
}

// CDN decides whether an asset sits behind an edge, and which one. Three sources,
// by how much they prove: the scanner's per-address membership (structural), a
// terminal CNAME, and last the resolved address's operator, which misfires (a
// cloud reselling edge capacity is not necessarily fronting).
//
// It is re-evaluated every pass: a target can move behind an edge between runs, and
// deciding once keeps the old answer forever.
func CDN(provider string, cnames []string, asnOrg string) (bool, string) {
	if provider != "" {
		return true, strings.ToLower(provider)
	}

	for _, cname := range cnames {
		name := strings.ToLower(strings.TrimSuffix(cname, "."))
		for suffix, known := range cdnCNAMESuffixes {
			if strings.HasSuffix(name, suffix) {
				return true, known
			}
		}
	}

	operator := strings.ToLower(asnOrg)
	for _, known := range cdnOperators {
		if strings.Contains(operator, known.needle) {
			return true, known.provider
		}
	}

	return false, ""
}
