package signals_test

import (
	"testing"

	"github.com/JoshuaMart/recon/internal/signals"
)

// The case that forces the whole package. On a fronted target no informative
// transport level failure ever happens: the edge always answers, with no
// refusal and no nxdomain, so an asset whose origin is dead would stay active
// forever.
func TestAnEdgeReportingADeadOriginIsADeath(t *testing.T) {
	t.Parallel()

	v := signals.Read(signals.Response{
		StatusCode: 530,
		Server:     "cloudflare",
		Title:      "acme.test | 530: Origin DNS error",
		Fronted:    true,
		Provider:   "cloudflare",
	})

	if v.Dead != "cloudflare_1016" {
		t.Fatalf("the origin error read as %q", v.Dead)
	}
	if v.Usable() {
		t.Error("the probe reached the edge and learned nothing about the service, so it measured nothing")
	}
	if v.Unclaimed == "" {
		t.Error("an origin DNS error is a takeover candidate as well as a death")
	}
}

// A 403 is not a 403. An application answering 403 on a protected route is
// alive and measured, and without the distinction either every 403 becomes a
// success and the regime switch never fires, or every 403 becomes a failure and
// every protected route in an inventory drifts toward unobservable.
func TestAnApplicationForbiddenIsASuccessAndAChallengeIsNot(t *testing.T) {
	t.Parallel()

	app := signals.Read(signals.Response{StatusCode: 403, Server: "nginx", Title: "Forbidden"})
	if app.Challenge != "" || app.Dead != "" {
		t.Fatalf("a plain 403 read as challenge=%q dead=%q", app.Challenge, app.Dead)
	}
	if !app.Usable() {
		t.Error("a 403 from an application is a measurement: the target answered and it is there")
	}

	challenge := signals.Read(signals.Response{
		StatusCode: 403, Server: "cloudflare", Title: "Just a moment...",
	})
	if challenge.Challenge == "" {
		t.Fatal("an interstitial was not recognized")
	}
	if challenge.Dead != "" {
		t.Error("a challenge is a live target, and it must never drift toward a death")
	}
	if challenge.Usable() {
		t.Error("the raw client measured nothing, so it must not count as a success either")
	}
}

// A technology in the WAF category means "there is a WAF here", not "this
// response is a mitigation". Reading it as proof of a challenge would mark
// every legitimate 403 of a fronted application unmeasurable.
func TestAFrontedTwoHundredIsNotAChallenge(t *testing.T) {
	t.Parallel()

	v := signals.Read(signals.Response{
		StatusCode: 200, Server: "cloudflare", Title: "Dashboard",
		Tech: []string{"Cloudflare", "React"}, Fronted: true, Provider: "cloudflare",
	})

	if v.Challenge != "" {
		t.Fatalf("a normal page behind an edge read as %q", v.Challenge)
	}
	if !v.Usable() {
		t.Error("a page a probe read perfectly well was counted as unmeasurable")
	}
}

func TestAnUnclaimedBucketIsAFindingRatherThanAMissingPath(t *testing.T) {
	t.Parallel()

	v := signals.Read(signals.Response{StatusCode: 404, Server: "AmazonS3"})
	takeover := signals.Unclaimed("https://files.acme.test/", v)
	if takeover == nil {
		t.Fatal("an unclaimed bucket produced no finding")
	}
	if takeover.Kind != signals.KindUnclaimedService || takeover.Signature != "s3" {
		t.Fatalf("the finding reads %+v", takeover)
	}

	// The server header is what makes it a signature. Without it the status
	// alone would match every missing path on the internet.
	plain := signals.Read(signals.Response{StatusCode: 404, Server: "nginx"})
	if signals.Unclaimed("https://acme.test/", plain) != nil {
		t.Error("an ordinary 404 was reported as a takeover candidate")
	}
}

// A plain nxdomain is a dead name, which is a death and not a finding. What
// makes it a takeover candidate is that the name still points somewhere, and
// that somewhere is claimable.
func TestADanglingCNAMENeedsTheCNAME(t *testing.T) {
	t.Parallel()

	found := signals.Dangling("dead", "nxdomain", []string{"old.acme.test", "bucket.s3.example.net."})
	if found == nil {
		t.Fatal("a dangling CNAME produced no finding")
	}
	if found.Target != "bucket.s3.example.net" {
		t.Errorf("the target is %q, and the last hop is the name somebody would register", found.Target)
	}
	if found.Kind != signals.KindOrphanCNAME || found.Signature != "nxdomain" {
		t.Errorf("the finding reads %+v", found)
	}

	if signals.Dangling("dead", "nxdomain", nil) != nil {
		t.Error("a bare nxdomain was reported as a takeover candidate")
	}
	if signals.Dangling("dead", "no_answer", []string{"x.acme.test"}) != nil {
		t.Error("a name that exists without an address was reported as a takeover candidate")
	}
	if signals.Dangling("live", "", []string{"x.acme.test"}) != nil {
		t.Error("a name that resolves was reported as a takeover candidate")
	}
}

func TestTheEdgeIsReadFromWhicheverSourceCanSeeIt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider string
		cnames   []string
		asnOrg   string
		fronted  bool
		want     string
	}{
		{"the scanner decided", "Cloudflare", nil, "", true, "cloudflare"},
		{"the terminal name", "", []string{"a.acme.test", "acme.edgekey.net."}, "", true, "akamai"},
		{"the operator", "", nil, "Amazon CloudFront", true, "cloudfront"},
		{"nothing says so", "", []string{"origin.acme.test"}, "Hetzner Online GmbH", false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			fronted, provider := signals.CDN(c.provider, c.cnames, c.asnOrg)
			if fronted != c.fronted || provider != c.want {
				t.Fatalf("got (%v, %q), want (%v, %q)", fronted, provider, c.fronted, c.want)
			}
		})
	}
}

// 522 is an origin connection timeout and is the most common of the three in
// practice. The comment beside the guard named it and the guard did not match
// it, so a fronted asset whose origin is gone stayed alive forever.
func TestEveryOriginUnreachableCodeIsADeath(t *testing.T) {
	t.Parallel()

	for _, status := range []int{521, 522, 523} {
		v := signals.Read(signals.Response{
			StatusCode: status, Server: "cloudflare", Fronted: true, Provider: "cloudflare",
		})
		if v.Dead == "" {
			t.Errorf("%d read as a live origin, and on a fronted target it is the only death signal there is", status)
		}
		if v.Usable() {
			t.Errorf("%d counted as a measurement of the service", status)
		}
	}
}
