// Package signals reads what a response means, as opposed to what it contains.
//
// Three questions are answered here and nowhere else: is this asset fronted, is
// this response a mitigation, and is this response an edge reporting that its
// origin is gone. All three are derived at ingestion from the payload, never
// believed from the scanner, for the reason P6 gives.
//
// What is readable today is bounded by what a report carries. A raw probe hands
// back a status, a server header, a title and a technology list, so the
// signatures below are the ones expressible in those four. The body based half,
// a challenge page recognized by its own text, arrives with the fingerprint
// layer.
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
	// Fronted is what the scanner determined about the address before it
	// connected, which is structural and cheap. It is evidence, not a verdict:
	// what it decides here is only which signatures are worth applying.
	Fronted  bool
	Provider string
}

// Verdict is what one response is worth.
type Verdict struct {
	// Dead names the origin error signature, empty when there is none. It is
	// an informative failure on the http layer: the probe reached the edge,
	// and the edge reported that the origin is gone.
	Dead string
	// Challenge names the mitigation signature, empty when there is none. The
	// target is alive, so it must never drift toward a death, and the probe
	// measured nothing, so it must not count as a success either.
	Challenge string
	Vendor    string
	// Unclaimed names a service answering that nobody owns this name. It is
	// the half of a takeover candidate that DNS cannot see.
	Unclaimed string
}

// Usable reports whether the observer got something out of this response.
//
// It is orthogonal to the outcome, which qualifies the target. A 403 carrying a
// mitigation signature is a target that answered and is there, and a probe that
// learned nothing, and those two facts drive different columns.
func (v Verdict) Usable() bool { return v.Challenge == "" && v.Dead == "" }

// Read applies every signature to one response.
func Read(r Response) Verdict {
	var v Verdict

	server := strings.ToLower(r.Server)
	title := strings.ToLower(r.Title)

	switch {
	// Cloudflare answers 530 for the whole 100x family, and the number is in
	// the page rather than in the status. 1016 is an origin DNS error, which
	// is a takeover candidate as much as a death; 1001 is a resolution error.
	case r.StatusCode == 530 && (strings.Contains(server, "cloudflare") || strings.Contains(title, "cloudflare")):
		v.Dead = "cloudflare_origin_dns"
		v.Vendor = "cloudflare"
		if strings.Contains(title, "1016") || strings.Contains(title, "origin dns") {
			v.Dead = "cloudflare_1016"
			v.Unclaimed = "cloudflare_1016"
		}

	// The edge is up, the origin is not answering at all. 521, 522 and 523 are
	// a refused connection, a timeout and an unreachable origin.
	case (r.StatusCode == 521 || r.StatusCode == 523) && strings.Contains(server, "cloudflare"):
		v.Dead = "cloudflare_origin_down"
		v.Vendor = "cloudflare"

	// Akamai reports a dead origin as an error page carrying a reference
	// number. The status alone would match every missing path on the internet,
	// so the server header is what makes it a signature.
	case strings.Contains(server, "akamaighost") &&
		(strings.Contains(title, "reference #") || strings.Contains(title, "an error occurred")):
		v.Dead = "akamai_reference"
		v.Vendor = "akamai"

	// Nobody has claimed the bucket. The service answers, so nothing at the
	// transport level will ever say this, and the name still resolves.
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

// mitigation recognizes a response that is the mitigation itself.
//
// A 403 is not a 403: an application answering 403 on a protected route is
// alive and measured, which is a success. Only a 403 carrying a mitigation
// signature is a challenge. Without the distinction one of two errors is
// certain, and both are expensive on roughly a tenth of a real perimeter.
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

	// A rate limit is the target telling the observer to stop. The target is
	// alive and the probe measured nothing about the service, which is the
	// same pair of facts as a challenge.
	case r.StatusCode == 429:
		return "rate_limited", vendorOf(r, server)
	}

	// A technology in the WAF category means "there is a WAF here", not "this
	// response is a mitigation". Cloudflare is reported on every response it
	// fronts, including a normal 200, so reading it as proof of a challenge
	// would mark every legitimate 403 of a fronted application unmeasurable.
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

// cdnCNAMESuffixes are the terminal names that say an asset is fronted even
// when the address enrichment says nothing.
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

// cdnOperators are the operator names that carry the same meaning as a
// terminal CNAME when the address was enriched but the name is direct.
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

// CDN decides whether an asset sits behind an edge, and which one.
//
// Three sources, in order of how much they prove. The scanner determines
// membership per address before it connects, which is the structural answer.
// A terminal CNAME is next. The operator of the resolved address is last,
// because it is the one that misfires: a name hosted on a cloud whose operator
// resells edge capacity is not necessarily fronted.
//
// It is re-evaluated on every pass rather than frozen. A target can move behind
// an edge between two runs, and an inventory that decided once reads the old
// answer forever.
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
