package enrich_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/JoshuaMart/recon/internal/enrich"
)

// A deployment with no database is a normal deployment, so this is a working
// implementation rather than a nil to check for at every call site.
func TestNothingIsUsableRatherThanAbsent(t *testing.T) {
	t.Parallel()

	e, err := enrich.Open(enrich.Paths{})
	if err != nil {
		t.Fatalf("open with no paths: %v", err)
	}
	defer func() { _ = e.Close() }()

	if e.Configured() {
		t.Error("an unconfigured enricher reports itself as configured")
	}
	if got := e.Lookup(netip.MustParseAddr("1.1.1.1")); !got.Empty() {
		t.Errorf("lookup = %+v, want nothing", got)
	}
}

// Somebody configured that path. Failing silently would leave them with an
// inventory that quietly has no operators in it.
func TestAPathThatIsSetAndUnreadableIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := enrich.Open(enrich.Paths{ASN: filepath.Join(t.TempDir(), "absent.mmdb")}); err == nil {
		t.Error("a missing database was accepted")
	}

	// And a file that exists without being one.
	path := filepath.Join(t.TempDir(), "not-a-database.mmdb")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := enrich.Open(enrich.Paths{ASN: path}); err == nil {
		t.Error("a file that is not a MaxMind database was accepted")
	}
}
