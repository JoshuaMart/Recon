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

	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
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
