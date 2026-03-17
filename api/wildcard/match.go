package wildcard

import "strings"

// Match returns true if fqdn matches the wildcard pattern.
// Pattern must be in the form "*.domain.tld".
// *.example.com matches sub.example.com but NOT sub.sub.example.com and NOT example.com.
func Match(pattern, fqdn string) bool {
	if pattern == "" || fqdn == "" {
		return false
	}

	if !strings.HasPrefix(pattern, "*.") {
		return false
	}

	// Extract the base domain: "*.example.com" -> ".example.com"
	suffix := pattern[1:]

	// fqdn must end with the suffix and have exactly one label before it
	if !strings.HasSuffix(fqdn, suffix) {
		return false
	}

	// Extract the part before the suffix
	prefix := fqdn[:len(fqdn)-len(suffix)]

	// Must be non-empty (not the root domain) and contain no dots (single level only)
	return prefix != "" && !strings.Contains(prefix, ".")
}
