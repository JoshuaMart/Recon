package normalize_test

import (
	"errors"
	"testing"

	"github.com/JoshuaMart/recon/internal/normalize"
)

func TestFQDN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, in, want string
	}{
		{"lowercased", "API.Target.COM", "api.target.com"},
		{"trailing dot removed", "api.target.com.", "api.target.com"},
		{"surrounding space", "  api.target.com  ", "api.target.com"},
		// Omnipresent, and a strict hostname grammar would drop real assets.
		{"underscore labels", "_dmarc.target.com", "_dmarc.target.com"},
		{"idn to punycode", "münchen.example.com", "xn--mnchen-3ya.example.com"},
		{"already punycode", "xn--mnchen-3ya.example.com", "xn--mnchen-3ya.example.com"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			key, err := normalize.FQDN(c.in)
			if err != nil {
				t.Fatalf("FQDN(%q): %v", c.in, err)
			}
			if key.Value != c.want {
				t.Errorf("FQDN(%q) = %q, want %q", c.in, key.Value, c.want)
			}
			if key.Host != c.want {
				t.Errorf("host = %q, want %q: it is derivable from the key, so it is filled here", key.Host, c.want)
			}
		})
	}
}

func TestFQDNRefusals(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", ".", "localhost", "1.2.3.4", "api target.com", "a..b.com", "http://a.com"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			if key, err := normalize.FQDN(in); err == nil {
				t.Errorf("FQDN(%q) returned %q, want a refusal", in, key.Value)
			} else if !errors.Is(err, normalize.ErrInvalid) {
				t.Errorf("the error does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

func TestAddress(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"1.2.3.4", "1.2.3.4"},
		// One machine, not two assets.
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"2606:4700:0000:0000:0000:0000:0000:0001", "2606:4700::1"},
		{"2606:4700::1", "2606:4700::1"},
		{"2606:4700::0001", "2606:4700::1"},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()

			key, err := normalize.IP(c.in)
			if err != nil {
				t.Fatalf("IP(%q): %v", c.in, err)
			}
			if key.Value != c.want {
				t.Errorf("IP(%q) = %q, want %q", c.in, key.Value, c.want)
			}
		})
	}
}

func TestService(t *testing.T) {
	t.Parallel()

	cases := []struct {
		host        string
		port        int
		proto       string
		want, host2 string
	}{
		{"API.target.com", 443, "tcp", "api.target.com:443/tcp", "api.target.com"},
		{"api.target.com", 8080, "", "api.target.com:8080/tcp", "api.target.com"},
		// Bracketed, which is what makes the key parseable at all: without it,
		// finding the port means splitting on the last colon.
		{"2606:4700::1", 443, "tcp", "[2606:4700::1]:443/tcp", "2606:4700::1"},
	}

	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()

			key, err := normalize.Service(c.host, c.port, c.proto)
			if err != nil {
				t.Fatalf("Service: %v", err)
			}
			if key.Value != c.want {
				t.Errorf("value = %q, want %q", key.Value, c.want)
			}
			if key.Host != c.host2 {
				t.Errorf("host = %q, want %q", key.Host, c.host2)
			}
			if key.Port != c.port {
				t.Errorf("port = %d, want %d", key.Port, c.port)
			}
		})
	}
}

func TestURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, in, want string
		port           int
	}{
		{"default port omitted", "https://api.target.com:443/v1", "https://api.target.com/v1", 443},
		{"non default port kept", "https://api.target.com:8443/v1", "https://api.target.com:8443/v1", 8443},
		{"query and fragment removed", "https://api.target.com/v1?page=2#top", "https://api.target.com/v1", 443},
		{"root keeps its slash", "https://api.target.com", "https://api.target.com/", 443},
		{"trailing slash removed", "https://api.target.com/admin/", "https://api.target.com/admin", 443},
		{"redundant slashes collapsed", "https://api.target.com//a///b", "https://api.target.com/a/b", 443},
		{"dot segments resolved", "https://api.target.com/a/./b/../c", "https://api.target.com/a/c", 443},
		{"case preserved in the path", "https://API.target.com/Admin/Users", "https://api.target.com/Admin/Users", 443},
		// %2F is a slash inside a segment rather than a separator, and decoding
		// it would turn one segment into two.
		{"reserved encoding kept", "https://api.target.com/a%2Fb", "https://api.target.com/a%2Fb", 443},
		{"reserved hex case uniformized", "https://api.target.com/a%2fb", "https://api.target.com/a%2Fb", 443},
		{"unreserved encoding decoded", "https://api.target.com/%7Euser", "https://api.target.com/~user", 443},
		{"scheme lowercased", "HTTPS://api.target.com/v1", "https://api.target.com/v1", 443},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			key, err := normalize.URL(c.in)
			if err != nil {
				t.Fatalf("URL(%q): %v", c.in, err)
			}
			if key.Value != c.want {
				t.Errorf("URL(%q) = %q, want %q", c.in, key.Value, c.want)
			}
			if key.Port != c.port {
				t.Errorf("port = %d, want %d", key.Port, c.port)
			}
			if key.Host != "api.target.com" {
				t.Errorf("host = %q, want api.target.com", key.Host)
			}
		})
	}
}

func TestURLRefusals(t *testing.T) {
	t.Parallel()

	// A key with a scheme nobody probes would be an asset no verification run
	// could ever address.
	for _, in := range []string{"ftp://target.com/pub", "ssh://target.com", "/relative", "https://target.com:99999/"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			if key, err := normalize.URL(in); err == nil {
				t.Errorf("URL(%q) returned %q, want a refusal", in, key.Value)
			}
		})
	}
}

// The same input, twice, has to give the same key. It sounds obvious and it is
// the property everything else rests on: deduplication compares keys, so one
// that varies would write a second asset for a surface that already exists.
func TestNormalizationIsIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"https://API.Target.com:443//a/./b/../c/",
		"https://api.target.com/%7Euser",
		"https://api.target.com/a%2fb",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			once, err := normalize.URL(in)
			if err != nil {
				t.Fatalf("URL(%q): %v", in, err)
			}
			twice, err := normalize.URL(once.Value)
			if err != nil {
				t.Fatalf("URL(%q): %v", once.Value, err)
			}
			if once.Value != twice.Value {
				t.Errorf("normalizing twice moved the key: %q then %q", once.Value, twice.Value)
			}
		})
	}
}
