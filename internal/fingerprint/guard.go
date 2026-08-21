// Package fingerprint talks to the rendering service.
//
// The service executes JavaScript controlled by the target, so it sits alone on
// its own network and holds no credential: everything it needs arrives in the
// request. This package is the only thing that speaks to it.
package fingerprint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInternal is a target the control plane refuses to ask for.
//
// The rendering service refuses these itself, which is where the property
// belongs: a check on the caller is a convention and a check on the service is
// a property, and the service is not only ever called by this. Refusing here
// as well means the guard is exercised by two processes that fail differently,
// and it keeps a bug in the inventory from becoming a request to a metadata
// endpoint.
var ErrInternal = errors.New("the target resolves inside a range that is never rendered")

// blocked are the ranges a browser must never be pointed at.
//
// Link local is first because it is the one that costs a compromise rather than
// an embarrassment: cloud instance credentials live at 169.254.169.254.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"), // link local, instance credentials
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("127.0.0.0/8"), // loopback, which is the container itself
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"), // private, whatever network this was deployed onto
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("0.0.0.0/8"), // unspecified, which several stacks route to loopback
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier grade NAT, a private range in more than one cloud
}

// Internal reports whether an address is one this never renders.
//
// The IPv4-mapped form is unwrapped first. Without that, every rule above is
// walked around by writing the same address the other way, and the check that
// looks the most obviously correct is the one that does nothing.
func Internal(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	for _, prefix := range blocked {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// literal reads a host as an address, in every spelling a browser accepts.
//
// The canonical form is the one nobody writes by accident. Chrome still parses
// the legacy inet_aton spellings, so http://2130706433/ and http://0177.0.0.1/
// are loopback, and a check that only knows the dotted-quad lets both past to a
// resolver that will fail on them and to a guard that reads a failed resolution
// as "not my problem".
func literal(host string) (netip.Addr, bool) {
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return addr, true
	}
	return inetAton(host)
}

// inetAton parses the legacy forms: one to four parts, each decimal, octal with
// a leading zero, or hexadecimal with a leading 0x. The last part absorbs
// whatever the earlier ones did not.
func inetAton(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}

	values := make([]uint64, 0, len(parts))
	for _, part := range parts {
		value, ok := legacyNumber(part)
		if !ok {
			return netip.Addr{}, false
		}
		values = append(values, value)
	}

	// Every part but the last is one byte; the last takes the remaining ones.
	var total uint64
	for i, value := range values {
		last := i == len(values)-1
		width := uint(8)
		if last {
			width = uint(8 * (4 - i))
		}
		if width < 64 && value >= 1<<width {
			return netip.Addr{}, false
		}
		if last {
			total |= value
		} else {
			total |= value << (8 * (3 - uint(i)))
		}
	}
	if total > 1<<32-1 {
		return netip.Addr{}, false
	}

	return netip.AddrFrom4([4]byte{
		byte(total >> 24), byte(total >> 16), byte(total >> 8), byte(total),
	}), true
}

func legacyNumber(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base, digits := 10, part
	switch {
	case len(part) > 2 && (part[:2] == "0x" || part[:2] == "0X"):
		base, digits = 16, part[2:]
	case len(part) > 1 && part[0] == '0':
		base, digits = 8, part[1:]
	}
	value, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// Resolver is how a name becomes addresses, so a test does not need DNS.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// SystemResolver asks the host's resolver, briefly.
func SystemResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// Guard decides whether a URL may be handed to a browser.
type Guard struct{ resolve Resolver }

// NewGuard builds one. A nil resolver uses the host's.
func NewGuard(resolve Resolver) *Guard {
	if resolve == nil {
		resolve = SystemResolver
	}
	return &Guard{resolve: resolve}
}

// Check refuses a target that resolves inside a blocked range.
//
// A name is resolved rather than trusted. An inventory holds names, and a name
// pointing at an internal address is exactly the case worth catching: the
// literal forms are the ones nobody writes by accident.
//
// A resolution that fails is not a refusal. The browser will do its own, and
// turning a DNS failure here into a refusal would silently drop every render on
// a host whose resolver is having a bad minute.
func (g *Guard) Check(ctx context.Context, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %q is not a URL", ErrInternal, raw)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: %q names no host", ErrInternal, raw)
	}

	if addr, ok := literal(host); ok {
		if Internal(addr) {
			return fmt.Errorf("%w: %s", ErrInternal, addr)
		}
		return nil
	}

	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if Internal(addr) {
			return fmt.Errorf("%w: %s resolves to %s", ErrInternal, host, addr)
		}
	}
	return nil
}
