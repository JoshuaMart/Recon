package scope

import "testing"

func TestClassify(t *testing.T) {
	c := NewClassifier([]Rule{
		{Include, Apex, "target.com"},
		{Exclude, FQDN, "admin.target.com"},
		{Exclude, Apex, "dev.target.com"},
		{Exclude, URLPrefix, "https://www.target.com/private"},
		{Include, URLPrefix, "https://shop.other.com/app"},
		{Include, CIDR, "10.0.0.0/24"},
	})
	host := func(h string) Target { return Target{Kind: "fqdn", Key: h, Host: h} }
	for _, tc := range []struct {
		name string
		t    Target
		want string
	}{
		{"apex itself", host("target.com"), InScope},
		{"under the apex", host("api.target.com"), InScope},
		{"label boundary, not a string suffix", host("eviltarget.com"), Unknown},
		{"apex appended to another domain", host("target.com.evil.com"), Unknown},
		{"excluded host", host("admin.target.com"), OutOfScope},
		{"under an excluded apex", host("a.dev.target.com"), OutOfScope},
		{"a service inherits its host", Target{Kind: "service", Key: "admin.target.com:443/tcp", Host: "admin.target.com", Port: 443}, OutOfScope},
		{"a path excluded, its service still in", Target{Kind: "service", Key: "www.target.com:443/tcp", Host: "www.target.com", Port: 443}, InScope},
		{"the excluded path", Target{Kind: "url", Key: "https://www.target.com/private/x", Host: "www.target.com", Port: 443}, OutOfScope},
		{"a sibling path, segment boundary", Target{Kind: "url", Key: "https://www.target.com/privatebeta", Host: "www.target.com", Port: 443}, InScope},
		{"url_prefix include reaches its host", host("shop.other.com"), InScope},
		{"and its service", Target{Kind: "service", Key: "shop.other.com:443/tcp", Host: "shop.other.com", Port: 443}, InScope},
		{"not another port", Target{Kind: "service", Key: "shop.other.com:8080/tcp", Host: "shop.other.com", Port: 8080}, Unknown},
		{"nor another path", Target{Kind: "url", Key: "https://shop.other.com/admin", Host: "shop.other.com", Port: 443}, Unknown},
		{"nor a sibling host", host("api.other.com"), Unknown},
		{"an address in a network", Target{Kind: "ip", Key: "10.0.0.7", Host: "10.0.0.7"}, InScope},
		{"an address outside", Target{Kind: "ip", Key: "10.0.1.7", Host: "10.0.1.7"}, Unknown},
	} {
		if got := c.Classify(tc.t); got != tc.want {
			t.Errorf("%s: Classify(%s) = %s, want %s", tc.name, tc.t.Key, got, tc.want)
		}
	}
}

func TestURLHostPort(t *testing.T) {
	for in, want := range map[string]struct {
		host string
		port int
	}{
		"https://a.com/x":           {"a.com", 443},
		"http://a.com/":             {"a.com", 80},
		"https://a.com:8443/":       {"a.com", 8443},
		"https://[2606:4700::1]/":   {"2606:4700::1", 443},
		"https://[2606:4700::1]:8/": {"2606:4700::1", 8},
	} {
		h, p, ok := urlHostPort(in)
		if !ok || h != want.host || p != want.port {
			t.Errorf("urlHostPort(%q) = %q %d %v", in, h, p, ok)
		}
	}
}
