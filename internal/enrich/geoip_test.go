package enrich

import (
	"net/netip"
	"os"
	"testing"
)

// The GeoLite2 files are not in the repository; the test runs where they are.
func TestLookupWithTheLocalFiles(t *testing.T) {
	dir := "../../var/geoip"
	if _, err := os.Stat(dir + "/GeoLite2-ASN.mmdb"); err != nil {
		t.Skip("no GeoLite2 files in var/geoip")
	}
	g, err := Open(dir)
	if err != nil || g == nil {
		t.Fatalf("open: %v", err)
	}
	defer g.Close()
	n := g.Lookup(netip.MustParseAddr("1.1.1.1"))
	if n.ASN != 13335 || n.ASOrg == "" {
		t.Errorf("1.1.1.1 = %+v, want Cloudflare's AS13335", n)
	}
	if n := g.Lookup(netip.MustParseAddr("::ffff:8.8.8.8")); n.ASN != 15169 || n.Country != "US" {
		t.Errorf("a mapped 8.8.8.8 = %+v", n)
	}
	if n := g.Lookup(netip.MustParseAddr("10.0.0.1")); n.ASN != 0 {
		t.Errorf("a private address has no AS: %+v", n)
	}
}

func TestMissingFilesDisableEnrichment(t *testing.T) {
	g, err := Open(t.TempDir())
	if err != nil || g != nil {
		t.Fatalf("Open on an empty dir = %v, %v", g, err)
	}
	if n := g.Lookup(netip.MustParseAddr("1.1.1.1")); n != (Network{}) {
		t.Errorf("a nil GeoIP answered %+v", n)
	}
}
