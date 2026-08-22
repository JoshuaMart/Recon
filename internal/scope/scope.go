// Package scope decides what a program is allowed to look at.
//
// The perimeter is persistent and versioned rather than a per-run setting, and
// it is evaluated on every ingestion rather than only when a run starts. That
// is what lets a rule change reclassify history without rescanning, and what
// lets an out of scope result be kept instead of thrown away.
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
	// Unknown was reached through lineage and matches no rule. Stored,
	// displayed, never probed, and where acquisitions and shared
	// infrastructure show up.
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

// Target is what gets classified.
//
// Addresses are supplied separately because a CIDR rule cannot be answered
// from a key: an address is only known after resolution, which is also why a
// CIDR exclusion cannot be pushed down into a run and is applied here instead.
type Target struct {
	Key       normalize.Key
	Addresses []netip.Addr
}

// ErrInvalidRule is the base of every compilation failure, so a caller can tell
// a malformed perimeter from a failure to read one.
var ErrInvalidRule = errors.New("invalid scope rule")

// Unmatchable reports a pattern that compiles and can never match anything.
//
// It is separate from Compile on purpose, and the separation is the whole point.
// Compile has to keep accepting whatever is already in the table: it runs on the
// ingestion path, so a rule it refuses stops reports being written, and a
// perimeter somebody typed badly six months ago would become an outage rather
// than a correction. This is the write path's check, where the answer is a
// refusal to somebody who is looking at the form.
//
// The case that costs a week: an apex rule written as "*.target.com". The apex
// matcher covers the domain and everything under it by construction, so the
// glob is redundant at best; what it actually does is make the comparison
// literal, and no hostname is ever equal to "*.target.com" or a suffix of
// ".*.target.com". The rule is stored, reads as in force, matches nothing, and
// every asset the perimeter should have covered stays unknown and unprobed.
// That is a perimeter that lies, silently, which is what this whole chapter is
// built to prevent.
func Unmatchable(matcher, pattern string) error {
	switch matcher {
	case MatchApex, MatchFQDN:
	default:
		// A regex is meant to carry metacharacters, a CIDR cannot hold one, and
		// a url prefix is compared as written.
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

// Set is a compiled perimeter. Compiling once and classifying many times is
// what makes reclassifying a whole program affordable.
type Set struct {
	includes []compiled
	excludes []compiled
}

type compiled struct {
	rule    Rule
	pattern string
	re      *regexp.Regexp
	prefix  netip.Prefix
	// host is the name a url_prefix rule names, kept so an include can reach
	// the host its path sits on. Empty on every other matcher.
	host string
}

// Compile turns rows into a rule set, rejecting anything unusable.
//
// A pattern that cannot be compiled is refused here rather than skipped: a
// rule that silently does not apply is a perimeter that lies, and the two
// directions of that lie are lost coverage and a scan outside authorization.
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
			// RE2, so a pattern cannot backtrack into a denial of service on
			// the ingestion path.
			re, err := regexp.Compile("(?i)" + rule.Pattern)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %s: %v", ErrInvalidRule, rule.ID, err)
			}
			item.re = re

		case MatchURLPrefix:
			item.pattern = strings.TrimSpace(rule.Pattern)
			// The host is read out of the pattern once, here, so the matcher
			// does not parse a URL per asset on the ingestion path. A prefix
			// that will not parse keeps an empty host and stays what it always
			// was: a rule that matches URLs by prefix and nothing else.
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

// Classify answers for one asset.
//
// It reads the asset's **host**, not its key. A rule names a host, and a
// service key is host:port/proto: matching the rule against the key would put
// the host in scope and leave every service on it out, which is the same
// perimeter described twice with only one of them acted on. So a service takes
// the status of its host, and a URL takes the status of its service.
//
// One matcher goes the other way. A url_prefix rule is more specific than a
// host, so a URL can be excluded while the service carrying it stays in scope:
// a child may be stricter than its parent, never looser.
func (s *Set) Classify(target Target) Status {
	if s.matches(s.excludes, target) {
		return OutOfScope
	}
	if s.matches(s.includes, target) {
		return InScope
	}
	return Unknown
}

// Matched reports which rule decided, for the report a reclassification
// returns. Without it, an over-broad exclusion is a number rather than a
// pattern somebody can read.
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
		// The apex itself and everything under it. The dot is in the
		// comparison rather than in the pattern, so evil-target.com does not
		// come back under target.com.
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
		// The only matcher that reads the key rather than the host, and the one
		// place the two kinds of rule are not symmetric.
		//
		// As an exclusion it reads the key alone, which is what lets it be
		// stricter than the asset's parent: a path can be taken out while the
		// service carrying it stays in. A child may be stricter than its parent.
		//
		// As an inclusion it also reaches the host, and that is not the same
		// rule read backwards. A path is not reachable without the name it sits
		// on: an include that matched the URL alone would put in scope a thing
		// that can only exist once its host has been probed, and the host would
		// never be probed because nothing put it in scope. The loop closes on
		// itself and the perimeter reads as configured while covering nothing.
		// So declaring a path declares the name it is served from, and the
		// service that answers it.
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
