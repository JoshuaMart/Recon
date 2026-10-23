package canon

import "testing"

func TestFQDN(t *testing.T) {
	for in, want := range map[string]string{
		"API.Target.COM.":   "api.target.com",
		" api.target.com ":  "api.target.com",
		"_dmarc.target.com": "_dmarc.target.com",
		"bücher.example":    "xn--bcher-kva.example",
		"a-b.c-d.example":   "a-b.c-d.example",
	} {
		got, err := FQDN(in)
		if err != nil || got != want {
			t.Errorf("FQDN(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "localhost", "1.2.3.4", "[2606:4700::1]", "-a.example", "a..example", "a b.example", "*.example.com"} {
		if got, err := FQDN(in); err == nil {
			t.Errorf("FQDN(%q) = %q, want an error", in, got)
		}
	}
}

func TestWildcard(t *testing.T) {
	if got, err := Wildcard("*.Target.com"); err != nil || got != "target.com" {
		t.Errorf("Wildcard = %q, %v", got, err)
	}
	for _, in := range []string{"target.com", "*.target.*", "*target.com", "api.*.target.com", "*.*.target.com"} {
		if got, err := Wildcard(in); err == nil {
			t.Errorf("Wildcard(%q) = %q, want an error", in, got)
		}
	}
}

func TestPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.7/24":        "10.0.0.0/24",
		"10.0.0.1":           "10.0.0.1/32",
		"2606:4700::1":       "2606:4700::1/128",
		"::ffff:1.2.3.4":     "1.2.3.4/32",
		"::ffff:1.2.3.0/120": "1.2.3.0/24",
		"2606:4700:0::/32":   "2606:4700::/32",
	} {
		got, err := Prefix(in)
		if err != nil || got.String() != want {
			t.Errorf("Prefix(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"10.0.0.0/33", "example.com", ""} {
		if _, err := Prefix(in); err == nil {
			t.Errorf("Prefix(%q) accepted", in)
		}
	}
}

func TestURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Target.com:443/App/":           "https://target.com/App",
		"http://target.com:80":                  "http://target.com/",
		"https://target.com:8443/a//b/./c/../d": "https://target.com:8443/a/b/d",
		"https://target.com/a?x=1#frag":         "https://target.com/a",
		"https://target.com/%7euser/%2f/%2Fx":   "https://target.com/~user/%2F/%2Fx",
		"https://[::ffff:1.2.3.4]:8080/":        "https://1.2.3.4:8080/",
		"https://[2606:4700::1]/":               "https://[2606:4700::1]/",
	} {
		got, err := URL(in)
		if err != nil || got != want {
			t.Errorf("URL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://target.com/", "target.com/app", "https://user:pw@target.com/", "https://target.com:99999/", "https://target.com/%zz"} {
		if got, err := URL(in); err == nil {
			t.Errorf("URL(%q) = %q, want an error", in, got)
		}
	}
}
