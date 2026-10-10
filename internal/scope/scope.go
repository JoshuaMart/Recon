// Package scope decides what a program is allowed to look at.
//
// The perimeter is persistent and versioned, not a per-run setting, and is
// evaluated on every ingestion. A rule change thus reclassifies history without
// rescanning, and an out-of-scope result is kept instead of discarded.
package scope

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// Status is what a rule set concludes about an asset.
type Status string

const (
	// InScope matches an include rule and no exclude rule. Actively probed.
	InScope Status = "in_scope"
	// OutOfScope matches an exclude rule. Stored, never probed.
	OutOfScope Status = "out_of_scope"
	// Unknown was reached through lineage and matches no rule. Stored and displayed,
	// never probed; where acquisitions and shared infrastructure show up.
	Unknown Status = "unknown"
)

// Rule kinds and matchers, mirroring the columns they come from.
const (
	Include = "include"
	Exclude = "exclude"

	MatchApex      = "apex"
	MatchFQDN      = "fqdn"
	MatchCIDR      = "cidr"
	MatchRegex     = "regex"
	MatchURLPrefix = "url_prefix"
)

// Rule is one row of the perimeter.
type Rule struct {
	ID      string
	Kind    string
	Matcher string
	Pattern string
}

// Target is what gets classified. Addresses are supplied separately because a
// CIDR rule cannot be answered from a key: an address is known only after
// resolution, which is why a CIDR exclusion cannot be pushed into a run.
type Target struct {
	Key       normalize.Key
	Addresses []netip.Addr
}

// ErrInvalidRule is the base of every compilation failure, to tell a malformed
// perimeter from a failure to read one.
var ErrInvalidRule = errors.New("invalid scope rule")

// Unmatchable reports a pattern that compiles and can never match anything. Separate from
// Compile on purpose: Compile runs on the ingestion path and must keep accepting whatever is
// in the table, or a perimeter mistyped months ago becomes an outage. This is the write
// path's check, a refusal to someone looking at the form.
//
// The costly case is an apex rule written "*.target.com": the apex matcher already covers
// the domain and everything under it, and the glob makes the comparison literal, so the rule
// reads as in force, matches nothing and leaves the assets it should cover unknown and
// unprobed, a perimeter that lies silently.
func Unmatchable(matcher, pattern string) error {
	switch matcher {
	case MatchApex, MatchFQDN:
	default:
		// A regex is meant to carry metacharacters, a CIDR cannot hold one, and a url
		// prefix is compared as written.
		return nil
	}

	trimmed := strings.TrimSpace(pattern)
	if !strings.ContainsAny(trimmed, "*?[]") {
		return nil
	}
	if suffix, found := strings.CutPrefix(trimmed, "*."); found && suffix != "" {
		return fmt.Errorf("%w: %q never matches a host name. An %s rule already covers the "+
			"domain and everything under it, so write %q",
			ErrInvalidRule, trimmed, MatchApex, suffix)
	}
	return fmt.Errorf("%w: %q never matches a host name, because %s and %s compare a name "+
		"rather than a pattern. Use the regex matcher to match a shape",
		ErrInvalidRule, trimmed, MatchApex, MatchFQDN)
}

// Set is a compiled perimeter: compile once, classify many, which makes
// reclassifying a whole program affordable.
type Set struct {
	includes []compiled
	excludes []compiled
}

type compiled struct {
	rule    Rule
	pattern string
	re      *regexp.Regexp
	prefix  netip.Prefix
	// host is the name a url_prefix rule names, so an include can reach the host its
	// path sits on. Empty on other matchers.
	host string
}

// Compile turns rows into a rule set, rejecting anything unusable. An
// uncompilable pattern is refused, not skipped: a rule silently not applying is a
// perimeter that lies, through lost coverage or a scan outside authorization.
func Compile(rules []Rule) (*Set, error) {
	set := &Set{}

	for _, rule := range rules {
		item := compiled{rule: rule, pattern: strings.ToLower(strings.TrimSpace(rule.Pattern))}
		if item.pattern == "" {
			return nil, fmt.Errorf("%w: rule %s has an empty pattern", ErrInvalidRule, rule.ID)
		}

		switch rule.Matcher {
		case MatchApex, MatchFQDN:
			host, err := normalize.Hostname(item.pattern)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %s: %v", ErrInvalidRule, rule.ID, err)
			}
			item.pattern = host

		case MatchCIDR:
			prefix, err := netip.ParsePrefix(item.pattern)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %s: %q is not a CIDR", ErrInvalidRule, rule.ID, rule.Pattern)
			}
			item.prefix = prefix.Masked()

		case MatchRegex:
			// RE2, so a pattern cannot backtrack into a denial of service on the ingestion
			// path.
			re, err := regexp.Compile("(?i)" + rule.Pattern)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %s: %v", ErrInvalidRule, rule.ID, err)
			}
			item.re = re

		case MatchURLPrefix:
			item.pattern = strings.TrimSpace(rule.Pattern)
			// The host is read once here so the matcher does not parse a URL per asset. A
			// prefix that will not parse keeps an empty host and matches by prefix only.
			if parsed, err := url.Parse(item.pattern); err == nil {
				item.host = strings.ToLower(parsed.Hostname())
			}

		default:
			return nil, fmt.Errorf("%w: rule %s has matcher %q", ErrInvalidRule, rule.ID, rule.Matcher)
		}

		switch rule.Kind {
		case Include:
			set.includes = append(set.includes, item)
		case Exclude:
			set.excludes = append(set.excludes, item)
		default:
			return nil, fmt.Errorf("%w: rule %s is neither an include nor an exclude", ErrInvalidRule, rule.ID)
		}
	}

	return set, nil
}

// Classify answers for one asset. It reads the asset's host, not its key: a rule names a
// host and a service key is host:port/proto, so matching the key would put the host in scope
// and leave its services out. A service takes its host's status, a URL its service's;
// url_prefix goes the other way (more specific than a host), so a child may be stricter than
// its parent, never looser.
func (s *Set) Classify(target Target) Status {
	if s.matches(s.excludes, target) {
		return OutOfScope
	}
	if s.matches(s.includes, target) {
		return InScope
	}
	return Unknown
}

// Matched reports which rule decided, for the reclassification report; without
// it an over-broad exclusion is a number, not a readable pattern.
func (s *Set) Matched(target Target) (Rule, bool) {
	for _, item := range s.excludes {
		if item.matches(target) {
			return item.rule, true
		}
	}
	for _, item := range s.includes {
		if item.matches(target) {
			return item.rule, true
		}
	}
	return Rule{}, false
}

func (s *Set) matches(rules []compiled, target Target) bool {
	for _, item := range rules {
		if item.matches(target) {
			return true
		}
	}
	return false
}

func (c compiled) matches(target Target) bool {
	host := strings.ToLower(target.Key.Host)

	switch c.rule.Matcher {
	case MatchApex:
		// The apex and everything under it. The dot is in the comparison, not the
		// pattern, so evil-target.com does not match target.com.
		return host == c.pattern || strings.HasSuffix(host, "."+c.pattern)

	case MatchFQDN:
		return host == c.pattern

	case MatchRegex:
		return c.re.MatchString(host)

	case MatchCIDR:
		if addr, err := netip.ParseAddr(host); err == nil && c.prefix.Contains(addr) {
			return true
		}
		for _, addr := range target.Addresses {
			if c.prefix.Contains(addr) {
				return true
			}
		}
		return false

	case MatchURLPrefix:
		// The only matcher reading the key rather than the host, and the one asymmetry. As an
		// exclusion it reads the key alone, so a path can be taken out while its service stays in. As
		// an inclusion it also reaches the host: a path is unreachable without its name, and an
		// include matching the URL alone would put in scope something that exists only once its host
		// is probed, which nothing would put in scope. So declaring a path declares its host and the
		// service answering it.
		if target.Key.Kind == normalize.KindURL {
			return strings.HasPrefix(target.Key.Value, c.pattern)
		}
		if c.rule.Kind != Include || c.host == "" {
			return false
		}
		return target.Key.Host == c.host
	}

	return false
}
