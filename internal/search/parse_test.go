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

// A quoted literal that would read as something else printed bare stays
// quoted, so printing back never changes what a query means.
func TestPrintingBackKeepsMeaning(t *testing.T) {
	for _, in := range []string{`title:"<script>"`, `title:">x"`, `title:"1..2"`, `title:"/x/"`, `host:"a b"`} {
		n, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		again, err := Parse(n.String())
		if err != nil {
			t.Errorf("%q printed %q, which does not parse: %v", in, n.String(), err)
			continue
		}
		a, b := n.(Term).Values[0], again.(Term).Values[0]
		if a.Lit != b.Lit || a.Op != b.Op || a.To != b.To || a.Regex != b.Regex {
			t.Errorf("%q printed %q, which means %+v instead of %+v", in, n.String(), b, a)
		}
	}
}

func TestRegexWithAnEscapedSlash(t *testing.T) {
	n, err := Parse(`final_url:/\/admin\// status:200`)
	if err != nil {
		t.Fatal(err)
	}
	v := n.(And).Left.(Term).Values[0]
	if !v.Regex || v.Lit != `\/admin\/` {
		t.Errorf("regex = %+v", v)
	}
	if n.String() != `final_url:/\/admin\// status:200` {
		t.Errorf("prints %q", n.String())
	}
}
