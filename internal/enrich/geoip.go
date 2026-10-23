// Package enrich reads the network context of an address from local GeoLite2
// files: its AS and its country. No API is called per address.
package enrich

import (
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Network is what an address resolves to. Zero values mean unknown.
type Network struct {
	ASN     int
	ASOrg   string
	Country string // ISO 3166-1 alpha-2
}

// GeoIP holds the two databases. A nil *GeoIP answers nothing, so a
// deployment without the files simply has no enrichment.
type GeoIP struct {
	asn, city *maxminddb.Reader
}

// Open opens GeoLite2-ASN.mmdb and GeoLite2-City.mmdb (or GeoLite2-Country)
// under dir. Missing files are not an error: enrichment is optional.
func Open(dir string) (*GeoIP, error) {
	g := &GeoIP{}
	var err error
	if g.asn, err = openIfPresent(filepath.Join(dir, "GeoLite2-ASN.mmdb")); err != nil {
		return nil, err
	}
	for _, name := range []string{"GeoLite2-City.mmdb", "GeoLite2-Country.mmdb"} {
		if g.city, err = openIfPresent(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
		if g.city != nil {
			break
		}
	}
	if g.asn == nil && g.city == nil {
		return nil, nil
	}
	return g, nil
}

func openIfPresent(path string) (*maxminddb.Reader, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return maxminddb.Open(path)
}

// Close releases the databases.
func (g *GeoIP) Close() {
	if g == nil {
		return
	}
	if g.asn != nil {
		_ = g.asn.Close()
	}
	if g.city != nil {
		_ = g.city.Close()
	}
}

// Lookup returns what the databases know of addr.
func (g *GeoIP) Lookup(addr netip.Addr) Network {
	var n Network
	if g == nil || !addr.IsValid() {
		return n
	}
	addr = addr.Unmap()
	if g.asn != nil {
		var rec struct {
			Number uint   `maxminddb:"autonomous_system_number"`
			Org    string `maxminddb:"autonomous_system_organization"`
		}
		if err := g.asn.Lookup(addr).Decode(&rec); err == nil {
			n.ASN, n.ASOrg = int(rec.Number), rec.Org //nolint:gosec // AS numbers fit
		}
	}
	if g.city != nil {
		var rec struct {
			Country struct {
				ISO string `maxminddb:"iso_code"`
			} `maxminddb:"country"`
		}
		if err := g.city.Lookup(addr).Decode(&rec); err == nil {
			n.Country = rec.Country.ISO
		}
	}
	return n
}

// First is the network of the first address that resolves to something: the
// addresses are sorted, so the choice is stable from one pass to the next.
func (g *GeoIP) First(addrs []netip.Addr) Network {
	for _, a := range addrs {
		if n := g.Lookup(a); n.ASN != 0 || n.Country != "" {
			return n
		}
	}
	return Network{}
}
