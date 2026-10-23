package ingest

import (
	"slices"
	"strings"
)

// Signature is the page a CDN edge serves when the origin behind it is gone:
// the only death signal a fronted target gives. It matches on what the HTTP
// probe reports. Empty fields match anything.
type Signature struct {
	Name     string `koanf:"name"`
	Statuses []int  `koanf:"statuses"`
	Server   string `koanf:"server"` // substring, case-insensitive
	Title    string `koanf:"title"`  // substring, case-insensitive
}

// DefaultSignatures are Cloudflare's origin errors.
var DefaultSignatures = []Signature{
	{Name: "cloudflare origin down", Statuses: []int{521, 522, 523, 530}, Server: "cloudflare"},
}

// DeadOrigin reports whether an answer is a known dead-origin page. An unknown
// error page matches nothing: it says nothing about the origin.
func DeadOrigin(sigs []Signature, d HTTPData) bool {
	for _, s := range sigs {
		if len(s.Statuses) > 0 && !slices.Contains(s.Statuses, d.StatusCode) {
			continue
		}
		if s.Server != "" && !strings.Contains(strings.ToLower(d.Server), strings.ToLower(s.Server)) {
			continue
		}
		if s.Title != "" && !strings.Contains(strings.ToLower(d.Title), strings.ToLower(s.Title)) {
			continue
		}
		if len(s.Statuses) == 0 && s.Server == "" && s.Title == "" {
			continue
		}
		return true
	}
	return false
}
