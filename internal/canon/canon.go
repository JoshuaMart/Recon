// Package canon turns host names, URLs and networks into their canonical form,
// the one every comparison and every unique key uses.
package canon

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// idn maps a Unicode name to punycode without the strict hostname grammar:
// underscore labels (_dmarc, _domainkey) are real and must survive.
var idn = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false))

// FQDN returns the canonical form of a host name: lowercase, no trailing dot,
// punycode. It refuses IP literals and single labels.
func FQDN(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", errors.New("empty name")
	}
	if _, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return "", fmt.Errorf("%q is an address, not a name", s)
	}
	a, err := idn.ToASCII(s)
	if err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	a = strings.ToLower(a)
	if len(a) > 253 {
		return "", fmt.Errorf("%q is longer than 253 characters", s)
	}
	labels := strings.Split(a, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%q has a single label", s)
	}
	for _, l := range labels {
		if err := checkLabel(l); err != nil {
			return "", fmt.Errorf("%q: %w", s, err)
		}
	}
	return a, nil
}

func checkLabel(l string) error {
	if l == "" || len(l) > 63 {
		return fmt.Errorf("label %q must be 1 to 63 characters", l)
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return fmt.Errorf("label %q starts or ends with a hyphen", l)
	}
	for _, c := range l {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return fmt.Errorf("label %q holds %q", l, c)
		}
	}
	return nil
}

// Wildcard returns the apex of a "*.example.com" pattern. Any other use of *
// is refused: a rule has to name where enumeration starts.
func Wildcard(s string) (string, error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, "*.")
	if !ok || strings.Contains(rest, "*") {
		return "", fmt.Errorf("%q is not of the form *.example.com", s)
	}
	return FQDN(rest)
}

// Prefix returns the canonical network of a CIDR or of a single address, which
// becomes a /32 or a /128.
func Prefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return unmap(p.Addr()).Prefix(p.Bits() - mappedBits(p.Addr()))
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = unmap(a)
	return a.Prefix(a.BitLen())
}

// An IPv4-mapped address is the IPv4 machine, not a second one.
func unmap(a netip.Addr) netip.Addr { return a.Unmap() }

func mappedBits(a netip.Addr) int {
	if a.Is4In6() {
		return 96
	}
	return 0
}

// URL returns the canonical form of an http or https URL: lowercase scheme and
// host, no default port, normalized path, no query and no fragment.
func URL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%q is not an http or https URL", s)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q carries credentials", s)
	}
	host, err := urlHost(u.Hostname())
	if err != nil {
		return "", err
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q has an invalid port", s)
		}
		if (scheme == "http" && n != 80) || (scheme == "https" && n != 443) {
			host += ":" + strconv.Itoa(n)
		}
	}
	path, err := Path(u.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	return scheme + "://" + host + path, nil
}

func urlHost(h string) (string, error) {
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		if a.Is6() {
			return "[" + a.String() + "]", nil
		}
		return a.String(), nil
	}
	return FQDN(h)
}

// Path normalizes an escaped URL path: unreserved %XX decoded, other escapes
// kept with uppercase hex (%2F is a slash inside a segment), repeated slashes
// collapsed, dot segments resolved, case kept, trailing slash dropped except at
// the root.
func Path(escaped string) (string, error) {
	var out []string
	for _, seg := range strings.Split(escaped, "/") {
		seg, err := normalizeEscapes(seg)
		if err != nil {
			return "", err
		}
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/"), nil
}

func normalizeEscapes(seg string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		if seg[i] != '%' {
			b.WriteByte(seg[i])
			continue
		}
		if i+2 >= len(seg) {
			return "", fmt.Errorf("truncated escape in %q", seg)
		}
		v, err := strconv.ParseUint(seg[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("bad escape in %q", seg)
		}
		if c := byte(v); isUnreserved(c) {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(seg[i+1:i+3]))
		}
		i += 2
	}
	return b.String(), nil
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}
