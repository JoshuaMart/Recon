package wildcard

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		fqdn    string
		want    bool
	}{
		// Basic matches
		{"*.example.com", "sub.example.com", true},
		{"*.example.com", "test.example.com", true},

		// Multi-level subdomain should match
		{"*.example.com", "sub.sub.example.com", true},

		// Root domain should NOT match
		{"*.example.com", "example.com", false},

		// Different domain
		{"*.example.com", "sub.other.com", false},

		// Empty strings
		{"", "sub.example.com", false},
		{"*.example.com", "", false},
		{"", "", false},

		// Invalid pattern (no wildcard)
		{"example.com", "sub.example.com", false},

		// Multiple TLD levels
		{"*.example.co.uk", "sub.example.co.uk", true},
		{"*.example.co.uk", "sub.sub.example.co.uk", true},
		{"*.example.co.uk", "example.co.uk", false},

		// Partial match should NOT work
		{"*.example.com", "subexample.com", false},

		// Hyphenated subdomain
		{"*.example.com", "my-sub.example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.fqdn, func(t *testing.T) {
			got := Match(tt.pattern, tt.fqdn)
			if got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.fqdn, got, tt.want)
			}
		})
	}
}
