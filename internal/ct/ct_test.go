package ct_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ct"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
)

func claim(apex string) ct.Claim {
	return ct.Claim{OrgID: uuid.New(), ProgramID: uuid.New(), Apex: apex}
}

func apexes(claims []ct.Claim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, c.Apex)
	}
	return out
}

// The refusal the whole matcher rests on. A suffix test would let anybody put a
// name inside somebody else's perimeter by registering a domain, which is not a
// theoretical attack on a system that turns a match into a scan.
func TestTheWalkClimbsLabelsRatherThanMatchingASuffix(t *testing.T) {
	set := ct.NewSet([]ct.Claim{claim("target.com")})

	cases := []struct {
		host  string
		match bool
		why   string
	}{
		{"target.com", true, "the apex itself is under the apex"},
		{"staging.api.target.com", true, "and so is anything beneath it"},
		{"TARGET.COM", true, "the walk is case insensitive, like every name in this system"},
		{"target.com.", true, "a trailing dot is the same name"},
		{"target.com.evil.com", false, "a suffix test would match this, and it is somebody else's domain"},
		{"evil-target.com", false, "the dot is in the comparison rather than in the pattern"},
		{"nottarget.com", false, "same"},
		{"com", false, "walking past the apex must not match its parent"},
		{"", false, "an empty name is not a name"},
	}

	for _, c := range cases {
		got := len(set.Match(c.host)) > 0
		if got != c.match {
			t.Errorf("Match(%q) = %v, want %v: %s", c.host, got, c.match, c.why)
		}
	}
}

// An apex may sit under another programme's apex. Stopping at the first match
// would drop the outer one, and it would drop it in silence.
func TestTheWalkDoesNotStopAtTheFirstMatch(t *testing.T) {
	inner, outer := claim("api.target.com"), claim("target.com")
	set := ct.NewSet([]ct.Claim{inner, outer})

	got := set.Match("staging.api.target.com")
	if len(got) != 2 {
		t.Fatalf("staging.api.target.com matched %v, and both apexes claim it", apexes(got))
	}
	if got[0].Apex != "api.target.com" || got[1].Apex != "target.com" {
		t.Errorf("the walk returned %v, and it climbs from the name outwards", apexes(got))
	}

	// The control: the inner apex claims nothing above itself.
	if got := set.Match("www.target.com"); len(got) != 1 || got[0].Apex != "target.com" {
		t.Errorf("www.target.com matched %v, and api.target.com is not above it", apexes(got))
	}
}

// Two programmes may legitimately hold the same apex, and the platform treats
// that as two assets and two runs rather than joining them.
func TestOneApexClaimedByTwoProgrammesIsTwoClaims(t *testing.T) {
	first, second := claim("target.com"), claim("target.com")
	set := ct.NewSet([]ct.Claim{first, second})

	got := set.Match("www.target.com")
	if len(got) != 2 {
		t.Fatalf("got %d claims, want one per programme", len(got))
	}
	if got[0].ProgramID == got[1].ProgramID {
		t.Error("the two claims name the same programme")
	}
	if set.Apexes() != 1 {
		t.Errorf("the set holds %d apexes for one name", set.Apexes())
	}
}

// The invariant that keeps this from being a second perimeter engine.
//
// The scope package answers "does this host match this rule" by walking the
// rules; this answers "which apexes does this host fall under" by walking the
// labels. They are two indexes over one predicate, and the day they disagree,
// Certificate Transparency starts creating assets the perimeter would not have
// classified in scope, or stops creating ones it would have.
//
// Run over the real stream rather than over invented names, because the shapes
// that break a suffix comparison are the ones nobody thinks to write down.
func TestTheWalkAgreesWithTheScopeEngine(t *testing.T) {
	sans := fixtureSANs(t)

	// Apexes taken from the corpus itself, so most names have a real chance of
	// matching, plus the adversarial ones a real set would never hold.
	patterns := map[string]struct{}{
		"target.com": {}, "com": {}, "co.uk": {}, "evil.com": {},
	}
	for i, san := range sans {
		if i%7 != 0 {
			continue
		}
		host := strings.TrimPrefix(san, "*.")
		if labels := strings.Split(host, "."); len(labels) >= 2 {
			patterns[strings.Join(labels[len(labels)-2:], ".")] = struct{}{}
			if len(labels) >= 3 {
				patterns[strings.Join(labels[len(labels)-3:], ".")] = struct{}{}
			}
		}
	}

	var claims []ct.Claim
	var rules []scope.Rule
	for pattern := range patterns {
		host, err := normalize.Hostname(pattern)
		if err != nil {
			continue
		}
		claims = append(claims, claim(host))
		rules = append(rules, scope.Rule{
			ID: host, Kind: scope.Include, Matcher: scope.MatchApex, Pattern: host,
		})
	}

	set := ct.NewSet(claims)
	perimeter, err := scope.Compile(rules)
	if err != nil {
		t.Fatalf("compile the perimeter: %v", err)
	}

	// The real corpus carries no adversarial name, because certificates are
	// issued for names people own. Left at that, this test passes just as
	// happily on a suffix comparison, which is the one fault it exists to
	// catch: a corpus without the discriminating case measures nothing.
	probes := append([]string(nil), sans...)
	for pattern := range patterns {
		probes = append(probes,
			pattern+".evil.com",
			"not"+pattern,
			"evil-"+pattern,
			"x."+pattern,
			pattern,
		)
	}

	compared := 0
	for _, san := range probes {
		host := strings.TrimPrefix(san, "*.")
		key, err := normalize.FQDN(host)
		if err != nil {
			continue
		}
		compared++

		walked := len(set.Match(key.Host)) > 0
		classified := perimeter.Classify(scope.Target{Key: key}) == scope.InScope
		if walked != classified {
			t.Fatalf("%q: the label walk says %v and the scope engine says %v",
				key.Host, walked, classified)
		}
	}
	if compared < 50 {
		t.Fatalf("only %d names were comparable, so this asserted almost nothing", compared)
	}
	t.Logf("%d names agreed across %d apexes, adversarial spellings included", compared, set.Apexes())
}

// A wildcard names no host, so it is counted and creates nothing. It is the
// blind spot rather than an omission, and it is 22.8 % of the SANs on the wire.
func TestAWildcardIsCountedAndNamesNothing(t *testing.T) {
	set := ct.NewSet([]ct.Claim{claim("target.com")})

	got, malformed := set.Sightings([]string{"*.target.com", "*.api.target.com", "www.target.com"})
	if malformed != 0 {
		t.Fatalf("%d SANs were unusable", malformed)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sightings, want one per SAN", len(got))
	}

	for _, s := range got[:2] {
		if !s.Wildcard {
			t.Errorf("%+v is not marked as a wildcard", s)
		}
		if s.Name != "" {
			t.Errorf("a wildcard carried the name %q, and it reveals no host", s.Name)
		}
	}
	if got[2].Wildcard || got[2].Name != "www.target.com" {
		t.Errorf("the ordinary name came back as %+v", got[2])
	}
}

// The subject's common name is routinely also the first SAN.
func TestANameRepeatedInOneCertificateIsOneSighting(t *testing.T) {
	set := ct.NewSet([]ct.Claim{claim("target.com")})

	got, _ := set.Sightings([]string{"www.target.com", "www.target.com", "WWW.target.com."})
	if len(got) != 2 {
		t.Fatalf("got %d sightings for %d spellings of one name", len(got), 3)
	}
	// The exact repeat is dropped on the raw string; the third spelling costs a
	// normalization and lands on the same key, which is what deduplication on
	// write is for rather than this.
	if got[0].Name != got[1].Name {
		t.Errorf("two spellings produced two names: %q and %q", got[0].Name, got[1].Name)
	}
}

func TestASetWithNoApexMatchesNothing(t *testing.T) {
	empty := ct.NewSet(nil)
	if got := empty.Match("www.target.com"); got != nil {
		t.Errorf("an empty set matched %v", apexes(got))
	}
	if got, _ := empty.Sightings([]string{"www.target.com"}); got != nil {
		t.Errorf("an empty set produced %d sightings", len(got))
	}
}

// The whole fixture, decoded. This is the assertion the struct exists for: a
// field renamed or moved on the feed's side fails here rather than producing a
// zero value nobody notices.
func TestTheRealStreamDecodes(t *testing.T) {
	frames := fixture(t)
	if len(frames) < 50 {
		t.Fatalf("the fixture holds %d frames", len(frames))
	}

	var precerts, certificates, wildcards, empties, withIssuer, withSource int
	for i, f := range frames {
		if f.MessageType != ct.MessageCertificate {
			t.Fatalf("frame %d carries message_type %q", i, f.MessageType)
		}
		switch f.Data.UpdateType {
		case ct.EntryPrecert:
			precerts++
		case ct.EntryCertificate:
			certificates++
		default:
			t.Fatalf("frame %d carries update_type %q, which is neither entry type", i, f.Data.UpdateType)
		}

		if len(f.Data.LeafCert.AllDomains) == 0 {
			empties++
		}
		for _, san := range f.Data.LeafCert.AllDomains {
			if strings.HasPrefix(san, "*.") {
				wildcards++
			}
		}
		if f.Data.LeafCert.Issuer.CN != "" || f.Data.LeafCert.Issuer.O != "" {
			withIssuer++
		}
		if f.Data.Source.Name != "" {
			withSource++
		}
	}

	// Both entry types have to be present, or the constant that names the one
	// that is missing is asserted by nothing.
	if precerts == 0 || certificates == 0 {
		t.Errorf("the fixture holds %d precertificates and %d certificates, and both are on the wire",
			precerts, certificates)
	}
	if wildcards == 0 {
		t.Error("no wildcard in the fixture, so the case it exists to cover is untested")
	}
	if empties == 0 {
		t.Error("no certificate without a DNS name, and that is the case a decoder gets wrong first")
	}
	// The lineage is the reason this stream is dialled instead of the cheap one.
	if withIssuer != len(frames) || withSource != len(frames) {
		t.Errorf("%d frames carry an issuer and %d carry a source, out of %d: the lineage is "+
			"why /domains-only was refused", withIssuer, withSource, len(frames))
	}
}

// A certificate carrying no DNS name arrives with an empty list rather than
// with something to refuse, and it must produce nothing at all.
func TestACertificateWithNoDNSNameProducesNothing(t *testing.T) {
	set := ct.NewSet([]ct.Claim{claim("target.com")})

	var found bool
	for _, f := range fixture(t) {
		if len(f.Data.LeafCert.AllDomains) != 0 {
			continue
		}
		found = true
		got, malformed := set.Sightings(f.Data.LeafCert.AllDomains)
		if len(got) != 0 || malformed != 0 {
			t.Errorf("a certificate with no DNS name produced %d sightings and %d malformed",
				len(got), malformed)
		}
	}
	if !found {
		t.Skip("the fixture holds no certificate without a DNS name")
	}
}

func fixture(t *testing.T) []ct.Frame {
	t.Helper()

	file, err := os.Open("testdata/stream.jsonl")
	if err != nil {
		t.Fatalf("open the pinned stream: %v", err)
	}
	defer func() { _ = file.Close() }()

	var frames []ct.Frame
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame ct.Frame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode frame %d: %v", len(frames), err)
		}
		frames = append(frames, frame)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the pinned stream: %v", err)
	}
	return frames
}

func fixtureSANs(t *testing.T) []string {
	t.Helper()

	var sans []string
	for _, f := range fixture(t) {
		sans = append(sans, f.Data.LeafCert.AllDomains...)
	}
	if len(sans) == 0 {
		t.Fatal("the pinned stream carries no SAN at all")
	}
	return sans
}

// A scope rule is stored exactly as somebody typed it: the console validates a
// throwaway copy and writes the original. So the set has to canonicalize, and it
// has to do it the way the perimeter does, or an apex is held in a spelling no
// SAN will ever match while the set reports itself as holding it.
func TestTheSetCanonicalizesAnApexTheWayThePerimeterDoes(t *testing.T) {
	cases := []struct {
		pattern string
		host    string
		why     string
	}{
		{"ACME.test", "staging.acme.test", "a rule typed in capitals"},
		{" acme.test ", "staging.acme.test", "a rule with the spaces a form leaves"},
		{"acme.test.", "staging.acme.test", "a fully qualified rule with its root dot"},
		{"café.test", "www.xn--caf-dma.test", "an IDN rule, which the perimeter holds in punycode"},
	}

	for _, c := range cases {
		set := ct.NewSet([]ct.Claim{claim(c.pattern)})
		if got := set.Match(c.host); len(got) == 0 {
			t.Errorf("%s: %q holds no claim on %q", c.why, c.pattern, c.host)
		}
	}

	// And a pattern that is not a name at all is dropped rather than held in a
	// spelling nothing matches. It cannot be in the perimeter either.
	if set := ct.NewSet([]ct.Claim{claim("not a host name")}); set.Apexes() != 0 {
		t.Errorf("an unusable pattern is in the set, which reports %d apexes", set.Apexes())
	}
}
