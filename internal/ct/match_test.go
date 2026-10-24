package ct

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func index(apexes ...string) (Index, map[string]uuid.UUID) {
	ix, ids := Index{}, map[string]uuid.UUID{}
	for _, a := range apexes {
		id := uuid.New()
		ids[a] = id
		ix.Add(Target{WildcardID: id, ProgramID: id, Apex: a})
	}
	return ix, ids
}

func TestMatchWalksLabels(t *testing.T) {
	ix, _ := index("target.com", "api.target.com", "other.io")
	for _, tc := range []struct {
		san      string
		name     string
		hits     int
		wildcard bool
	}{
		{"staging.api.target.com", "staging.api.target.com", 2, false},
		{"Target.COM.", "target.com", 1, false},
		{"target.com.evil.com", "", 0, false}, // a suffix match would take it
		{"eviltarget.com", "", 0, false},
		{"*.target.com", "target.com", 1, true},
		{"nothing.example", "", 0, false},
		{"bad_label!.target.com", "", 0, false},
	} {
		name, targets, wildcard := ix.Match(tc.san)
		if name != tc.name || len(targets) != tc.hits || wildcard != tc.wildcard {
			t.Errorf("%s = %q, %d targets, wildcard %v; want %q, %d, %v", tc.san, name, len(targets), wildcard, tc.name, tc.hits, tc.wildcard)
		}
	}
}

func TestParseReadsTheLiteFormat(t *testing.T) {
	c, err := Parse([]byte(`{"data":{"leaf_cert":{"all_domains":["a.t.com","*.t.com"],"issuer":{"CN":"YE2","O":"Let's Encrypt"}},
		"seen":1792839164.5,"source":{"name":"Google Argon2027h1"}},"message_type":"certificate_update"}`))
	if err != nil || len(c.Names) != 2 || c.Issuer != "Let's Encrypt YE2" || c.Log != "Google Argon2027h1" || c.Seen.Unix() != 1792839164 {
		t.Fatalf("parse = %+v %v", c, err)
	}
	if _, err := Parse([]byte(`{"message_type":"heartbeat"}`)); !errors.Is(err, errNotCertificate) {
		t.Fatalf("heartbeat = %v", err)
	}
}
