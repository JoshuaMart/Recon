package search

import "testing"

func TestParsePrintsBack(t *testing.T) {
	for in, want := range map[string]string{
		"tech:nginx@<1.25 status:2xx port:8080,8443 -cdn:cloudflare": "tech:nginx@<1.25 status:2xx port:8080,8443 -cdn:cloudflare",
		"tech:wordpress@5.0..6.1 program:acme* first_seen:<7d":       "tech:wordpress@5.0..6.1 program:acme* first_seen:<7d",
		"(status:401 OR status:403) has:favicon -triage:ignored":     "(status:401 OR status:403) has:favicon -triage:ignored",
		`title:"Sign in"`:      `title:"Sign in"`,
		"host:/^dev[0-9]+\\./": "host:/^dev[0-9]+\\./",
		"NOT (a:1 b:2)":        "-(a:1 b:2)",
		"a:1 OR b:2 c:3":       "a:1 OR b:2 c:3",
		"tech:jquery@?":        "tech:jquery@?",
		"tech:php@7,8.0":       "tech:php@7,8.0",
		"status:400..499":      "status:400..499",
		"  TITLE:x  ":          "title:x",
	} {
		n, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got := n.String(); got != want {
			t.Errorf("Parse(%q) prints %q, want %q", in, got, want)
		}
		// Printing is the query: parsing it again gives the same text.
		again, err := Parse(n.String())
		if err != nil || again.String() != n.String() {
			t.Errorf("round trip of %q: %v %v", in, again, err)
		}
	}
}

func TestPrecedence(t *testing.T) {
	n, _ := Parse("a:1 OR b:2 c:3")
	or, ok := n.(Or)
	if !ok {
		t.Fatalf("top = %T, want Or: AND binds tighter", n)
	}
	if _, ok := or.Right.(And); !ok {
		t.Errorf("right = %T, want And", or.Right)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"nginx", "a:", "(a:1", "title:\"x", "host:/x", "has:a,b", "a:1 )", ":x"} {
		if n, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, n)
		}
	}
	if n, err := Parse("   "); err != nil || n != nil {
		t.Errorf("an empty query is everything: %v %v", n, err)
	}
}
