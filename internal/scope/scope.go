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
		// The only matcher that reads the key rather than the host, and the
		// only one that can be stricter than the asset's parent.
		if target.Key.Kind != normalize.KindURL {
			return false
		}
		return strings.HasPrefix(target.Key.Value, c.pattern)
	}

	return false
}
