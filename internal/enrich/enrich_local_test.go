//go:build integration

package enrich_test

import (
	"net/netip"
	"os"
	"testing"

	"github.com/JoshuaMart/recon/internal/enrich"
)

// Against the real databases when they are present. Skipped rather than failed
// when they are not: a checkout has no reason to carry seventy megabytes of
// binary, and a test that cannot run is not a test that failed.
func TestTheRealDatabasesAnswer(t *testing.T) {
	const (
		city = "../../var/geoip/GeoLite2-City.mmdb"
		asn  = "../../var/geoip/GeoLite2-ASN.mmdb"
	)

	for _, path := range []string{city, asn} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("no database at %s", path)
		}
	}

	e, err := enrich.Open(enrich.Paths{City: city, ASN: asn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = e.Close() }()

	if !e.Configured() {
		t.Fatal("the enricher does not report itself as configured")
	}

	// An ordinary address answers with both.
	unicast := e.Lookup(netip.MustParseAddr("8.8.8.8"))
	if unicast.ASN == 0 || unicast.Country == "" {
		t.Errorf("8.8.8.8 = %+v, want an operator and a country", unicast)
	}
	t.Logf("8.8.8.8 -> AS%d %s, %s %s", unicast.ASN, unicast.ASNOrg, unicast.Country, unicast.City)

	// An anycast address answers with an operator and no country, and that is
	// the case this test exists for. The record carries registered_country and
	// nothing else, so falling back to it would report Australia for an address
	// served from every continent. The operator is the actionable half and it
	// is the one that survives.
	anycast := e.Lookup(netip.MustParseAddr("1.1.1.1"))
	if anycast.ASN == 0 {
		t.Errorf("1.1.1.1 = %+v, want an operator", anycast)
	}
	if anycast.Country != "" {
		t.Errorf("1.1.1.1 reported country %q: a registration is not a location, and the "+
			"country exists to spot an asset somewhere unusual for its programme",
			anycast.Country)
	}
	t.Logf("1.1.1.1 -> AS%d %s, country %q", anycast.ASN, anycast.ASNOrg, anycast.Country)
}
