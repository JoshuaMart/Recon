// Package scope holds scope rules, how a ScopesExtractor value becomes one, and
// the sync that keeps synced programs in line with ScopesExtractor.
package scope

import (
	"fmt"
	"strings"

	"github.com/JoshuaMart/recon/internal/canon"
)

// Rule kinds and matchers, as stored in scope_rule.
const (
	Include = "include"
	Exclude = "exclude"

	Apex      = "apex"
	FQDN      = "fqdn"
	URLPrefix = "url_prefix"
	CIDR      = "cidr"
)

// Rule is what a scope rule means, independent of when it is in force.
type Rule struct {
	Kind    string
	Matcher string
	Pattern string // canonical
}

func (r Rule) String() string { return r.Kind + " " + r.Matcher + " " + r.Pattern }

// ParseRule validates and canonicalizes a rule typed by hand. An apex accepts
// both "example.com" and "*.example.com".
func ParseRule(kind, matcher, pattern string) (Rule, error) {
	if kind != Include && kind != Exclude {
		return Rule{}, fmt.Errorf("kind %q is not include or exclude", kind)
	}
	var (
		p   string
		err error
	)
	switch matcher {
	case Apex:
		if strings.HasPrefix(strings.TrimSpace(pattern), "*") {
			p, err = canon.Wildcard(pattern)
		} else {
			p, err = canon.FQDN(pattern)
		}
	case FQDN:
		p, err = canon.FQDN(pattern)
	case URLPrefix:
		p, err = canon.URL(pattern)
	case CIDR:
		var n interface{ String() string }
		n, err = canon.Prefix(pattern)
		if err == nil {
			p = n.String()
		}
	default:
		return Rule{}, fmt.Errorf("matcher %q is not one of apex, fqdn, url_prefix, cidr", matcher)
	}
	if err != nil {
		return Rule{}, err
	}
	return Rule{Kind: kind, Matcher: matcher, Pattern: p}, nil
}

// trackedTypes are the ScopesExtractor types whose values can be a host, a
// URL or a network. Others (mobile, executable, ...) are untracked even when the
// value looks like a name: com.example.app is an Android package, not a host.
var trackedTypes = map[string]bool{
	"": true, "web": true, "api": true, "wildcard": true, "domain": true,
	"url": true, "cidr": true, "ip": true, "ip_address": true, "network": true,
}

// FromScopeValue turns one ScopesExtractor entry into a rule. When it cannot,
// the rule is zero and reason says why, for the untracked list.
func FromScopeValue(value, typ string, inScope bool) (rule Rule, reason string) {
	kind := Include
	if !inScope {
		kind = Exclude
	}
	t := strings.ToLower(strings.TrimSpace(typ))
	if !trackedTypes[t] {
		return Rule{}, fmt.Sprintf("type %q is not tracked", typ)
	}
	v := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(v, "*"):
		apex, err := canon.Wildcard(v)
		if err != nil {
			return Rule{}, err.Error()
		}
		return Rule{Kind: kind, Matcher: Apex, Pattern: apex}, ""
	case strings.Contains(v, "://"):
		return fromURL(kind, v)
	}
	if n, err := canon.Prefix(v); err == nil {
		return Rule{Kind: kind, Matcher: CIDR, Pattern: n.String()}, ""
	}
	name, err := canon.FQDN(v)
	if err == nil {
		return Rule{Kind: kind, Matcher: FQDN, Pattern: name}, ""
	}
	// A path or a port without a scheme (api.example.com/graphql) names a URL,
	// and https is what a program almost always means.
	if strings.ContainsAny(v, "/:") {
		if r, reason := fromURL(kind, "https://"+v); reason == "" {
			return r, ""
		}
	}
	return Rule{}, "not a host, URL, wildcard or network: " + err.Error()
}

// fromURL keeps a URL as a url_prefix only when it names a path or a port. A
// bare root on the default port (https://www.example.com/) names the host.
func fromURL(kind, v string) (Rule, string) {
	u, err := canon.URL(v)
	if err != nil {
		return Rule{}, err.Error()
	}
	_, rest, _ := strings.Cut(u, "://")
	host, path, _ := strings.Cut(rest, "/")
	hasPort := strings.Contains(host, ":")
	if strings.HasPrefix(host, "[") {
		hasPort = strings.Contains(host, "]:")
	}
	if path != "" || hasPort {
		return Rule{Kind: kind, Matcher: URLPrefix, Pattern: u}, ""
	}
	if n, err := canon.Prefix(strings.Trim(host, "[]")); err == nil {
		return Rule{Kind: kind, Matcher: CIDR, Pattern: n.String()}, ""
	}
	return Rule{Kind: kind, Matcher: FQDN, Pattern: host}, ""
}
