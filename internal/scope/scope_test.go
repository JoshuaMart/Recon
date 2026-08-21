package scope_test

import (
	"net/netip"
	"testing"

	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
)

func compile(t *testing.T, rules ...scope.Rule) *scope.Set {
	t.Helper()

	set, err := scope.Compile(rules)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return set
}

func rule(kind, matcher, pattern string) scope.Rule {
	return scope.Rule{ID: matcher + ":" + pattern, Kind: kind, Matcher: matcher, Pattern: pattern}
}

func fqdn(t *testing.T, name string) scope.Target {
	t.Helper()

	key, err := normalize.FQDN(name)
	if err != nil {
		t.Fatalf("FQDN(%q): %v", name, err)
	}
	return scope.Target{Key: key}
}

func service(t *testing.T, host string, port int) scope.Target {
	t.Helper()

	key, err := normalize.Service(host, port, "tcp")
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	return scope.Target{Key: key}
}

func url(t *testing.T, raw string) scope.Target {
	t.Helper()

	key, err := normalize.URL(raw)
	if err != nil {
		t.Fatalf("URL(%q): %v", raw, err)
	}
	return scope.Target{Key: key}
}

func TestAnApexCoversItselfAndWhatIsUnderIt(t *testing.T) {
	t.Parallel()

	set := compile(t, rule(scope.Include, scope.MatchApex, "target.com"))

	for _, name := range []string{"target.com", "api.target.com", "a.b.target.com"} {
		if got := set.Classify(fqdn(t, name)); got != scope.InScope {
			t.Errorf("%s = %s, want in scope", name, got)
		}
	}
	// The dot is in the comparison rather than in the pattern, so a name that
	// merely ends with the same letters does not come back under it.
	if got := set.Classify(fqdn(t, "evil-target.com")); got != scope.Unknown {
		t.Errorf("evil-target.com = %s, want unknown", got)
	}
}

func TestAnExcludeBeatsAnInclude(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchFQDN, "admin.target.com"),
	)

	if got := set.Classify(fqdn(t, "admin.target.com")); got != scope.OutOfScope {
		t.Errorf("admin.target.com = %s, want out of scope", got)
	}
	if got := set.Classify(fqdn(t, "api.target.com")); got != scope.InScope {
		t.Errorf("api.target.com = %s, want in scope", got)
	}
}

// The defect this reading exists to prevent: a rule names a host, and a service
// key is host:port/proto. Matching the rule against the key would put the host
// in scope and leave every service on it out, which is the same perimeter
// described twice with only one of them acted on.
func TestAServiceInheritsItsHost(t *testing.T) {
	t.Parallel()

	set := compile(t, rule(scope.Include, scope.MatchApex, "target.com"))

	for _, target := range []scope.Target{
		service(t, "api.target.com", 443),
		service(t, "api.target.com", 8080),
		url(t, "https://api.target.com/v1"),
	} {
		if got := set.Classify(target); got != scope.InScope {
			t.Errorf("%s = %s, want in scope: it is on a host the perimeter names",
				target.Key.Value, got)
		}
	}
}

func TestExcludingAHostTakesItsServicesWithIt(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchFQDN, "admin.target.com"),
	)

	if got := set.Classify(service(t, "admin.target.com", 443)); got != scope.OutOfScope {
		t.Errorf("the service of an excluded host = %s, want out of scope: it would "+
			"otherwise keep its due dates and go on being scanned", got)
	}
}

// The one matcher that goes the other way. A child may be stricter than its
// parent, never looser.
func TestAURLPrefixExclusionLeavesItsServiceInScope(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchURLPrefix, "https://app.target.com/internal/"),
	)

	if got := set.Classify(url(t, "https://app.target.com/internal/admin")); got != scope.OutOfScope {
		t.Errorf("the excluded path = %s, want out of scope", got)
	}
	if got := set.Classify(url(t, "https://app.target.com/public")); got != scope.InScope {
		t.Errorf("another path on the same service = %s, want in scope", got)
	}
	if got := set.Classify(service(t, "app.target.com", 443)); got != scope.InScope {
		t.Errorf("the service carrying the excluded path = %s, want in scope: a "+
			"url_prefix rule is more specific than a host", got)
	}
}

// Where acquisitions, affiliated domains and shared infrastructure show up.
func TestWhatMatchesNothingIsUnknownRatherThanExcluded(t *testing.T) {
	t.Parallel()

	set := compile(t, rule(scope.Include, scope.MatchApex, "target.com"))

	if got := set.Classify(fqdn(t, "cdn.thirdparty.net")); got != scope.Unknown {
		t.Errorf("cdn.thirdparty.net = %s, want unknown: it is kept and displayed, "+
			"never probed, and it is a candidate for a scope extension", got)
	}
}

func TestACIDRRuleReadsTheAddress(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchCIDR, "10.0.0.0/8"),
	)

	// On an address asset, from the key itself.
	ip, err := normalize.IP("10.1.2.3")
	if err != nil {
		t.Fatalf("IP: %v", err)
	}
	if got := set.Classify(scope.Target{Key: ip}); got != scope.OutOfScope {
		t.Errorf("10.1.2.3 = %s, want out of scope", got)
	}

	// And on a name, from what it resolved to. An address is only known after
	// resolution, which is why this rule cannot be pushed into a run.
	target := fqdn(t, "internal.target.com")
	target.Addresses = []netip.Addr{netip.MustParseAddr("10.4.5.6")}
	if got := set.Classify(target); got != scope.OutOfScope {
		t.Errorf("a name resolving inside the range = %s, want out of scope", got)
	}
}

func TestARegexIsCaseInsensitiveAndAnchoredByTheAuthor(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchRegex, `^(staging|preprod)[0-9]*\.`),
	)

	for _, name := range []string{"staging.target.com", "STAGING2.target.com", "preprod9.target.com"} {
		if got := set.Classify(fqdn(t, name)); got != scope.OutOfScope {
			t.Errorf("%s = %s, want out of scope", name, got)
		}
	}
	if got := set.Classify(fqdn(t, "prestaging.target.com")); got != scope.InScope {
		t.Errorf("prestaging.target.com = %s, want in scope: the author anchored the pattern", got)
	}
}

// A rule that silently does not apply is a perimeter that lies, and the two
// directions of that lie are lost coverage and a scan outside authorization.
func TestAnUnusableRuleIsRefusedRatherThanSkipped(t *testing.T) {
	t.Parallel()

	cases := []scope.Rule{
		rule(scope.Include, scope.MatchRegex, "^(unclosed"),
		rule(scope.Include, scope.MatchCIDR, "10.0.0.0/33"),
		rule(scope.Include, "hostname", "target.com"),
		rule("allow", scope.MatchApex, "target.com"),
		rule(scope.Include, scope.MatchApex, "   "),
	}

	for _, r := range cases {
		t.Run(r.Matcher+"/"+r.Kind, func(t *testing.T) {
			t.Parallel()

			if _, err := scope.Compile([]scope.Rule{r}); err == nil {
				t.Errorf("%+v compiled, want a refusal", r)
			}
		})
	}
}

func TestMatchedNamesTheRuleThatDecided(t *testing.T) {
	t.Parallel()

	set := compile(t,
		rule(scope.Include, scope.MatchApex, "target.com"),
		rule(scope.Exclude, scope.MatchFQDN, "admin.target.com"),
	)

	matched, ok := set.Matched(fqdn(t, "admin.target.com"))
	if !ok {
		t.Fatal("no rule reported, and one decided")
	}
	// An over-broad exclusion has to be readable as a pattern rather than as a
	// number of assets that moved.
	if matched.Pattern != "admin.target.com" {
		t.Errorf("pattern = %q, want the exclusion that decided", matched.Pattern)
	}
}
