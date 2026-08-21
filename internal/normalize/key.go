// Package normalize owns the canonical forms.
//
// Deduplication depends entirely on it, which is why one function here decides
// each shape and SQL never parses a key. A second implementation, in a query or
// in a migration, would diverge on the cases that matter: an IPv6 literal, an
// implicit port, a percent-encoded separator, and nothing would raise an error
// when the two disagreed.
package normalize

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Kind is what an asset is.
type Kind string

const (
	// KindFQDN is a name.
	KindFQDN Kind = "fqdn"
	// KindIP is an address.
	KindIP Kind = "ip"
	// KindService is a host and a port, which is the unit of a web surface.
	KindService Kind = "service"
	// KindURL is what a human declares. No producer creates one: a path is
	// where a redirect landed that day, and an application renaming its login
	// page would retire one asset and create another.
	KindURL Kind = "url"
)

// Key is a canonical identity, together with what is derivable from it.
//
// Host, Port and Scheme are filled here rather than waited for from an
// observation: the information is in the key at the moment the row is written,
// and deriving it in SQL later would be the second implementation this package
// exists to prevent.
type Key struct {
	Kind   Kind
	Value  string
	Host   string
	Port   int
	Scheme string
}

// ErrInvalid is the base of every rejection here, so a caller can tell a
// malformed input from a failure to reach something.
var ErrInvalid = errors.New("invalid asset key")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// defaultPorts is what a scheme implies. A key omits the port when it matches,
// which is not cosmetic: it makes a scheme on a non-default port the only kind
// that keeps one, so the unusual case stands out instead of drowning among
// redundant suffixes.
var defaultPorts = map[string]int{"http": 80, "https": 443}

// Hostname normalizes a name or an address literal.
//
// Lowercased, trailing dot removed, IDN converted to punycode. Underscore
// labels are accepted on purpose: _dmarc and _domainkey are everywhere, and a
// strict hostname grammar would drop real assets.
func Hostname(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", invalid("empty host")
	}

	// A bracketed literal is an address, and lowercasing it before parsing is
	// what makes two spellings of the same IPv6 converge.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		addr, err := Address(host[1 : len(host)-1])
		if err != nil {
			return "", err
		}
		return addr, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return Address(host)
	}

	host = strings.ToLower(host)
	if strings.ContainsAny(host, " \t/?#@") {
		return "", invalid("host %q contains a character a hostname cannot hold", raw)
	}
	if strings.Contains(host, "..") {
		return "", invalid("host %q has an empty label", raw)
	}

	ascii, err := idna.ToASCII(host)
	if err != nil {
		return "", invalid("host %q is not a usable name: %v", raw, err)
	}
	return ascii, nil
}

// Address normalizes an IP literal. An IPv4-mapped address is reduced to its
// IPv4 form: ::ffff:1.2.3.4 and 1.2.3.4 are one machine, not two assets.
func Address(raw string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "", invalid("%q is not an IP address", raw)
	}
	if four := ip.To4(); four != nil {
		return four.String(), nil
	}
	return ip.String(), nil
}

// FQDN builds a name key.
func FQDN(raw string) (Key, error) {
	host, err := Hostname(raw)
	if err != nil {
		return Key{}, err
	}
	if net.ParseIP(host) != nil {
		return Key{}, invalid("%q is an address, not a name", raw)
	}
	if !strings.Contains(host, ".") {
		return Key{}, invalid("%q is not a fully qualified name", raw)
	}
	return Key{Kind: KindFQDN, Value: host, Host: host}, nil
}

// IP builds an address key.
func IP(raw string) (Key, error) {
	addr, err := Address(raw)
	if err != nil {
		return Key{}, err
	}
	return Key{Kind: KindIP, Value: addr, Host: addr}, nil
}

// Service builds a host and port key, which is the unit of a web surface.
func Service(host string, port int, proto string) (Key, error) {
	normalized, err := Hostname(host)
	if err != nil {
		return Key{}, err
	}
	if port < 1 || port > 65535 {
		return Key{}, invalid("port %d is out of range", port)
	}

	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return Key{}, invalid("protocol %q is neither tcp nor udp", proto)
	}

	return Key{
		Kind:  KindService,
		Value: fmt.Sprintf("%s:%d/%s", bracket(normalized), port, proto),
		Host:  normalized,
		Port:  port,
	}, nil
}

// URL builds a declared surface key.
//
// The query string is excluded. Placeholders do not fix it: the order of
// parameters is arbitrary and optional ones produce several keys for one
// endpoint. The deeper reason is that the inventory is about surfaces rather
// than requests, so ?page=2 is not a second asset.
func URL(raw string) (Key, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Key{}, invalid("%q is not a URL: %v", raw, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if _, ok := defaultPorts[scheme]; !ok {
		return Key{}, invalid("scheme %q is not a web scheme", parsed.Scheme)
	}

	host, err := Hostname(parsed.Hostname())
	if err != nil {
		return Key{}, err
	}

	port := defaultPorts[scheme]
	if raw := parsed.Port(); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Key{}, invalid("port %q is out of range", raw)
		}
	}

	// EscapedPath rather than Path: the decoded form has already lost the
	// difference between a separator and a %2F inside a segment.
	value := scheme + "://" + bracket(host)
	if port != defaultPorts[scheme] {
		value += ":" + strconv.Itoa(port)
	}
	value += Path(parsed.EscapedPath())

	return Key{Kind: KindURL, Value: value, Host: host, Port: port, Scheme: scheme}, nil
}

// Path normalizes the path of a URL.
//
// Case is preserved, because paths are case sensitive where hosts are not.
// Redundant slashes go, "." and ".." are resolved, and a trailing slash is
// removed everywhere but at the root.
func Path(raw string) string {
	if raw == "" || raw == "/" {
		return "/"
	}

	// Split before decoding. A %2F is a slash *inside* a segment rather than a
	// separator, so decoding first would turn one segment into two and change
	// the path.
	var out []string
	for _, segment := range strings.Split(raw, "/") {
		decoded := decodeUnreserved(segment)
		switch decoded {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, decoded)
		}
	}

	if len(out) == 0 {
		return "/"
	}
	return "/" + strings.Join(out, "/")
}

// decodeUnreserved expands the percent-encodings that carry no meaning and
// leaves the ones that do, uniformizing the hexadecimal case of what stays so
// that two spellings of the same reserved character converge.
func decodeUnreserved(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		if segment[i] != '%' || i+2 >= len(segment) {
			b.WriteByte(segment[i])
			continue
		}
		value, err := strconv.ParseUint(segment[i+1:i+3], 16, 8)
		if err != nil {
			// Not an encoding at all. Left as written rather than guessed at.
			b.WriteByte(segment[i])
			continue
		}
		if c := byte(value); isUnreserved(c) {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(segment[i+1:i+3]))
		}
		i += 2
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	}
	return false
}

// bracket wraps an IPv6 literal, which is what makes a service key parseable
// at all: without it, splitting on the last colon is the only way to find the
// port, and that is a rule waiting to be got wrong.
func bracket(host string) string {
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
