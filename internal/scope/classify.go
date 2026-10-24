package scope

import (
	"net/netip"
	"strconv"
	"strings"
)

// Scope statuses, as stored on assets.
const (
	InScope    = "in_scope"
	OutOfScope = "out_of_scope"
	Unknown    = "unknown"
)

// Target is what classification reads from an asset: its kind, its host and,
// for services and URLs, its port and scheme and its key.
type Target struct {
	Kind   string // fqdn, ip, service, url
	Key    string
	Host   string
	Port   int
	Scheme string
}

// Classifier holds the rules in force for one program.
type Classifier struct {
	include, exclude []Rule
}

// NewClassifier sorts the rules by kind.
func NewClassifier(rules []Rule) *Classifier {
	c := &Classifier{}
	for _, r := range rules {
		if r.Kind == Include {
			c.include = append(c.include, r)
		} else {
			c.exclude = append(c.exclude, r)
		}
	}
	return c
}

// Classify reads the host: a service takes the status of its host and a URL the
// status of its service. A url_prefix exclusion is the one rule that can make a
// child stricter than its parent.
func (c *Classifier) Classify(t Target) string {
	for _, r := range c.exclude {
		if excludes(r, t) {
			return OutOfScope
		}
	}
	for _, r := range c.include {
		if includes(r, t) {
			return InScope
		}
	}
	return Unknown
}

func excludes(r Rule, t Target) bool {
	if r.Matcher == URLPrefix {
		return t.Kind == "url" && urlUnder(t.Key, r.Pattern)
	}
	return hostMatches(r, t.Host)
}

func includes(r Rule, t Target) bool {
	if r.Matcher != URLPrefix {
		return hostMatches(r, t.Host)
	}
	// An include on a path reaches the name it is served from and the service
	// it answers on, and no further: a path is not reachable without them.
	host, port, ok := urlHostPort(r.Pattern)
	if !ok || t.Host != host {
		return false
	}
	switch t.Kind {
	case "url":
		return urlUnder(t.Key, r.Pattern)
	case "service":
		return t.Port == port
	default:
		return true
	}
}

func hostMatches(r Rule, host string) bool {
	switch r.Matcher {
	case Apex:
		return host == r.Pattern || strings.HasSuffix(host, "."+r.Pattern)
	case FQDN:
		return host == r.Pattern
	case CIDR:
		p, err := netip.ParsePrefix(r.Pattern)
		if err != nil {
			return false
		}
		a, err := netip.ParseAddr(strings.Trim(host, "[]"))
		return err == nil && p.Contains(a.Unmap())
	}
	return false
}

// urlUnder reports whether url sits under prefix on a path segment boundary:
// /app covers /app and /app/x, never /application.
func urlUnder(url, prefix string) bool {
	if url == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(url, prefix)
	}
	return strings.HasPrefix(url, prefix+"/")
}

// urlHostPort splits a canonical URL's authority, filling in the default port.
func urlHostPort(u string) (string, int, bool) {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return "", 0, false
	}
	authority, _, _ := strings.Cut(rest, "/")
	port := 443
	if scheme == "http" {
		port = 80
	}
	host := authority
	if i := strings.LastIndex(authority, ":"); i > strings.LastIndex(authority, "]") {
		p, err := strconv.Atoi(authority[i+1:])
		if err != nil {
			return "", 0, false
		}
		host, port = authority[:i], p
	}
	return strings.Trim(host, "[]"), port, true
}
