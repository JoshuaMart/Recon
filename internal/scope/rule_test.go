package scope

import (
	"strings"
	"testing"
)

func TestFromScopeValue(t *testing.T) {
	for _, tc := range []struct {
		value, typ string
		inScope    bool
		want       Rule
	}{
		{"*.Example.com", "web", true, Rule{Include, Apex, "example.com"}},
		{"api.example.com", "web", true, Rule{Include, FQDN, "api.example.com"}},
		{"https://example.com/app/", "web", true, Rule{Include, URLPrefix, "https://example.com/app"}},
		// A bare root names the host; a port or a path keeps it a URL.
		{"https://www.example.com/", "web", true, Rule{Include, FQDN, "www.example.com"}},
		{"http://www.example.com", "web", true, Rule{Include, FQDN, "www.example.com"}},
		{"https://example.com:8443/", "web", true, Rule{Include, URLPrefix, "https://example.com:8443/"}},
		{"https://10.0.0.1/", "web", true, Rule{Include, CIDR, "10.0.0.1/32"}},
		{"https://[2606:4700::1]/", "web", true, Rule{Include, CIDR, "2606:4700::1/128"}},
		{"https://[2606:4700::1]:8443/", "web", true, Rule{Include, URLPrefix, "https://[2606:4700::1]:8443/"}},
		// Without a scheme, a path or a port is an https URL.
		{"ideal.dbs.com/idealx", "web", true, Rule{Include, URLPrefix, "https://ideal.dbs.com/idealx"}},
		{"example.com:8443", "web", true, Rule{Include, URLPrefix, "https://example.com:8443/"}},
		{"10.0.0.0/24", "cidr", true, Rule{Include, CIDR, "10.0.0.0/24"}},
		{"10.0.0.1", "", true, Rule{Include, CIDR, "10.0.0.1/32"}},
		{"admin.example.com", "web", false, Rule{Exclude, FQDN, "admin.example.com"}},
		{"api.example.com", "API", true, Rule{Include, FQDN, "api.example.com"}},
	} {
		got, reason := FromScopeValue(tc.value, tc.typ, tc.inScope)
		if reason != "" || got != tc.want {
			t.Errorf("FromScopeValue(%q, %q) = %v, %q; want %v", tc.value, tc.typ, got, reason, tc.want)
		}
	}
}

func TestFromScopeValueUntracked(t *testing.T) {
	for _, tc := range []struct{ value, typ, reason string }{
		// Looks like a host, is an Android package: the type decides.
		{"com.example.app", "mobile", "not tracked"},
		{"Example Desktop App", "executable", "not tracked"},
		{"example.com (production only)", "web", "not a host"},
		{"*.example.*", "web", "not of the form"},
		{"ftp://example.com/", "web", "not an http"},
	} {
		got, reason := FromScopeValue(tc.value, tc.typ, true)
		if got != (Rule{}) || !strings.Contains(reason, tc.reason) {
			t.Errorf("FromScopeValue(%q, %q) = %v, %q; want untracked with %q", tc.value, tc.typ, got, reason, tc.reason)
		}
	}
}

func TestParseRule(t *testing.T) {
	for _, tc := range []struct {
		kind, matcher, pattern string
		want                   Rule
	}{
		{Include, Apex, "*.Example.com", Rule{Include, Apex, "example.com"}},
		{Include, Apex, "example.com.", Rule{Include, Apex, "example.com"}},
		{Exclude, CIDR, "192.168.1.7/16", Rule{Exclude, CIDR, "192.168.0.0/16"}},
		{Exclude, URLPrefix, "https://example.com/admin/", Rule{Exclude, URLPrefix, "https://example.com/admin"}},
	} {
		got, err := ParseRule(tc.kind, tc.matcher, tc.pattern)
		if err != nil || got != tc.want {
			t.Errorf("ParseRule(%q, %q, %q) = %v, %v; want %v", tc.kind, tc.matcher, tc.pattern, got, err, tc.want)
		}
	}
	for _, tc := range [][3]string{
		{"allow", Apex, "example.com"},
		{Include, "regex", "^a"},
		{Include, FQDN, "*.example.com"},
		{Include, URLPrefix, "example.com/app"},
	} {
		if _, err := ParseRule(tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("ParseRule(%v) accepted", tc)
		}
	}
}
