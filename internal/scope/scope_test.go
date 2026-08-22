package scope_test

import (
	"errors"
	"net/netip"
	"strings"
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

// A rule that compiles and can never match is worse than one that will not
// compile, because it announces nothing.
//
// The case this exists for cost a full inventory: an apex rule written as
// "*.jomar.ovh" was stored, read as in force, matched none of a hundred and
// seven assets, and left every one of them unknown and unscheduled. The queue
// then showed three zeroes under a discovery run that had just delivered a
// hundred and twenty-three observations, which reads as a broken scanner.
func TestAPatternThatCanNeverMatchIsRefused(t *testing.T) {
	t.Parallel()

	for _, refused := range []struct{ matcher, pattern string }{
		{scope.MatchApex, "*.target.test"},
		{scope.MatchApex, "*.sub.target.test"},
		{scope.MatchFQDN, "*.target.test"},
		{scope.MatchApex, "app-*.target.test"},
		{scope.MatchFQDN, "app?.target.test"},
	} {
		err := scope.Unmatchable(refused.matcher, refused.pattern)
		if err == nil {
			t.Errorf("%s %q was accepted, and it matches no host name",
				refused.matcher, refused.pattern)
			continue
		}
		if !errors.Is(err, scope.ErrInvalidRule) {
			t.Errorf("%s %q refused with %v, want an invalid rule", refused.matcher, refused.pattern, err)
		}
	}

	// The message points at what to write instead, because the mistake is
	// reasonable: every other tool in this space takes a glob here.
	err := scope.Unmatchable(scope.MatchApex, "*.target.test")
	if !strings.Contains(err.Error(), `"target.test"`) {
		t.Errorf("the refusal does not name the pattern to write instead: %v", err)
	}

	// The positive control, without which the above passes on a check that
	// refuses everything. A matcher meant to carry metacharacters keeps them,
	// and an ordinary name is untouched.
	for _, accepted := range []struct{ matcher, pattern string }{
		{scope.MatchApex, "target.test"},
		{scope.MatchFQDN, "app.target.test"},
		{scope.MatchRegex, `^app-\d+\.target\.test$`},
		{scope.MatchCIDR, "10.0.0.0/8"},
		{scope.MatchURLPrefix, "https://target.test/api"},
	} {
		if err := scope.Unmatchable(accepted.matcher, accepted.pattern); err != nil {
			t.Errorf("%s %q was refused: %v", accepted.matcher, accepted.pattern, err)
		}
	}
}

// And the perimeter it describes really does cover what the refusal tells
// somebody to write, which is the half that makes the advice worth giving.
func TestAnApexCoversTheDomainAndEverythingUnderIt(t *testing.T) {
	t.Parallel()

	set, err := scope.Compile([]scope.Rule{
		{ID: "1", Kind: scope.Include, Matcher: scope.MatchApex, Pattern: "target.test"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for _, host := range []string{"target.test", "app.target.test", "a.b.target.test"} {
		key, err := normalize.FQDN(host)
		if err != nil {
			t.Fatalf("key %s: %v", host, err)
		}
		if got := set.Classify(scope.Target{Key: key}); got != scope.InScope {
			t.Errorf("%s classified %s, want in scope", host, got)
		}
	}
	// The dot is in the comparison rather than in the pattern, so a name that
	// merely ends in the same letters does not come back under it.
	key, err := normalize.FQDN("eviltarget.test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if got := set.Classify(scope.Target{Key: key}); got == scope.InScope {
		t.Error("eviltarget.test came back in scope under target.test")
	}
}

// A url_prefix include reaches nothing on its own, and the shape of that is
// worth an assertion rather than a discovery.
//
// The matcher is the one that reads the key rather than the host, and it is
// built to be stricter than the asset's parent: a URL can be excluded while the
// service carrying it stays in scope. Used as an include it goes the other way,
// and the loop closes on itself. The URL is the only kind it can match; a URL
// exists once its service answered; a service is probed through its host; and
// the host is left unknown, because a rule that only matches URLs never matched
// it. Nothing is ever scheduled, so the URL that would have matched is never
// created.
func TestAURLPrefixIncludeReachesNoHost(t *testing.T) {
	t.Parallel()

	set := compile(t, scope.Rule{
		ID: "1", Kind: scope.Include, Matcher: scope.MatchURLPrefix, Pattern: "https://www.target.test",
	})

	host, err := normalize.FQDN("www.target.test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	service, err := normalize.Service("www.target.test", 443, "tcp")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	page, err := normalize.URL("https://www.target.test/")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	// The host is what carries a due date, and it is the one this rule cannot
	// reach. That is the whole of why the perimeter never starts.
	if got := set.Classify(scope.Target{Key: host}); got == scope.InScope {
		t.Errorf("the host classified %s: a url_prefix rule matches a URL and nothing else", got)
	}
	if got := set.Classify(scope.Target{Key: service}); got == scope.InScope {
		t.Errorf("the service classified %s, and it is observed through its host", got)
	}
	// And the one kind it does reach, which is what makes it useful as an
	// exclusion beside an include that covers the host.
	if got := set.Classify(scope.Target{Key: page}); got != scope.InScope {
		t.Errorf("the URL classified %s, want in scope", got)
	}

	// Beside an apex include, the whole chain comes in and the exclusion is the
	// direction this matcher is for: stricter than its parent, never looser.
	both := compile(t,
		scope.Rule{ID: "1", Kind: scope.Include, Matcher: scope.MatchApex, Pattern: "target.test"},
		scope.Rule{ID: "2", Kind: scope.Exclude, Matcher: scope.MatchURLPrefix, Pattern: "https://www.target.test/admin"},
	)
	if got := both.Classify(scope.Target{Key: host}); got != scope.InScope {
		t.Errorf("the host classified %s under an apex include, want in scope", got)
	}
	admin, err := normalize.URL("https://www.target.test/admin/users")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if got := both.Classify(scope.Target{Key: admin}); got != scope.OutOfScope {
		t.Errorf("the excluded path classified %s, want out of scope while its service stays in", got)
	}
	if got := both.Classify(scope.Target{Key: service}); got != scope.InScope {
		t.Errorf("the service classified %s: a child may be stricter than its parent, "+
			"and the parent does not follow it out", got)
	}
}
