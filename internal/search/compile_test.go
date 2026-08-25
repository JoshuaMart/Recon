package search

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func parse(t *testing.T, raw string) Node {
	t.Helper()

	node, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return node
}

// TestTheOrganizationIsEmittedOnEveryCompilation is the assertion the whole
// compiler rests on.
//
// This query does not live in the static query files, so the tenant guard does
// not see it. What holds instead is that the clause is structural, and this is
// what makes it so: it is asserted on every shape a tree can take, including
// the empty one, which is the shape an interface sends before anybody has
// clicked a facet.
func TestTheOrganizationIsEmittedOnEveryCompilation(t *testing.T) {
	t.Parallel()

	org := uuid.New()
	for name, tree := range map[string]string{
		"an empty request":  ``,
		"an empty group":    `{"op":"and"}`,
		"one clause":        `{"op":"eq","field":"port","value":443}`,
		"a nested negation": `{"op":"not","clauses":[{"op":"eq","field":"is_cdn","value":true}]}`,
		"a deep tree": `{"op":"or","clauses":[
			{"op":"and","clauses":[{"op":"eq","field":"port","value":80}]},
			{"op":"suffix","field":"key","value":".target.test"}]}`,
	} {
		compiled, err := Compile(org, parse(t, tree))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(compiled.SQL, Alias+".org_id = $1") {
			t.Errorf("%s compiles to %q, which does not open on the organization", name, compiled.SQL)
		}
		if len(compiled.Args) == 0 || compiled.Args[0] != org {
			t.Errorf("%s binds %v as its first parameter, want the organization", name, compiled.Args)
		}
	}
}

// And a compilation with no organization is a refusal rather than a query, so a
// caller that lost the principal cannot read the whole cluster.
func TestACompilationWithNoOrganizationIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := Compile(uuid.Nil, Node{Op: OpAnd}); err == nil {
		t.Fatal("a query compiled with no organization")
	}
}

// TestTheTenantCannotBeExpressed is the other half of the same property.
//
// An organization filter the caller can express is one the caller can forget,
// and one they can set to somebody else's.
func TestTheTenantCannotBeExpressed(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"org_id", "org", "tenant", "organization"} {
		_, err := Parse([]byte(`{"op":"eq","field":"` + name + `","value":"x"}`))
		if err == nil {
			t.Errorf("%q is a field the registry accepts", name)
		}
	}
}

// A programme is not the tenant wearing another name, and the difference is
// what decides whether it belongs in the vocabulary.
//
// The organization is emitted by the compiler on every compilation, so a query
// can neither name it nor omit it. A programme is a perimeter inside one
// organization, the switcher on every screen filters on it, and the tenant
// clause is still emitted beside it: naming somebody else's programme returns
// nothing rather than their inventory.
func TestAProgrammeIsAFilterAndTheTenantIsStillEmitted(t *testing.T) {
	t.Parallel()

	org := uuid.New()
	program := uuid.New()
	compiled, err := Compile(org, parse(t,
		`{"op":"eq","field":"program_id","value":"`+program.String()+`"}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(compiled.SQL, "c.org_id = $1") {
		t.Errorf("SQL = %q, and the tenant clause is not in it", compiled.SQL)
	}
	// Bound and cast, never interpolated. The cast is what refuses a malformed
	// identifier, with the value still a parameter.
	if !strings.Contains(compiled.SQL, "c.program_id = $2::uuid") {
		t.Errorf("SQL = %q, want the programme bound and cast", compiled.SQL)
	}
	if len(compiled.Args) != 2 || compiled.Args[0] != org || compiled.Args[1] != program.String() {
		t.Errorf("args = %v, want the organization then the programme", compiled.Args)
	}
}

func TestAnUnknownFieldOrOperatorIsARefusalRatherThanAQuery(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"an unknown field":                                `{"op":"eq","field":"severity","value":9}`,
		"an operator the field refuses":                   `{"op":"suffix","field":"port","value":"443"}`,
		"a title substring, which is not in the registry": `{"op":"contains","field":"title","value":"login"}`,
		"a leaf carrying clauses":                         `{"op":"eq","field":"port","clauses":[{"op":"and"}]}`,
		"a group carrying a field":                        `{"op":"and","field":"port"}`,
		"a negation with nothing to negate":               `{"op":"not"}`,
	}
	for name, tree := range cases {
		if _, err := Parse([]byte(tree)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestANestedEmptyGroupIsARefusalRatherThanAGuess.
//
// An empty AND is TRUE by identity and an empty OR is FALSE, so the same shape
// means the whole inventory in one parent and nothing at all in the other. The
// case that decides it is the negation: "not(and())" is "not everything", which
// is zero rows, and a compiler that lets an empty group contribute nothing
// answers the entire inventory instead. A console that cleared the last facet
// out of a negated group would read that as "there is nothing to exclude" and
// get everything back.
func TestANestedEmptyGroupIsARefusalRatherThanAGuess(t *testing.T) {
	t.Parallel()

	for name, tree := range map[string]string{
		"a negated empty group": `{"op":"not","clauses":[{"op":"and","clauses":[]}]}`,
		"an empty group beside a clause": `{"op":"or","clauses":[
			{"op":"eq","field":"port","value":443},{"op":"and","clauses":[]}]}`,
		"an empty group inside an and": `{"op":"and","clauses":[
			{"op":"eq","field":"port","value":443},{"op":"or","clauses":[]}]}`,
	} {
		if _, err := Parse([]byte(tree)); err == nil {
			t.Errorf("%s was accepted, and there is no reading of it that is not a guess", name)
		}
	}

	// And the one place it is meaningful stays meaningful: an interface that
	// has not filtered on anything yet.
	compiled, err := Compile(uuid.New(), parse(t, `{"op":"and","clauses":[]}`))
	if err != nil {
		t.Fatalf("an empty request at the root was refused: %v", err)
	}
	if compiled.SQL != Alias+".org_id = $1" {
		t.Errorf("an empty request compiles to %q, want the organization alone", compiled.SQL)
	}
}

// TestASuffixWithAWildcardStaysASuffix is the trap the doc names, and it is
// invisible without a wildcard in the value.
//
// Escaping the LIKE wildcards before reversing puts each backslash after the
// character it should protect, so "_" stays a live wildcard and a trailing
// backslash escapes the appended "%". The search silently becomes an equality.
func TestASuffixWithAWildcardStaysASuffix(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t,
		`{"op":"suffix","field":"key","value":".my_target.test"}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	pattern, ok := compiled.Args[1].(string)
	if !ok {
		t.Fatalf("the pattern is %T, want a string", compiled.Args[1])
	}

	// Reversed first, then escaped: the value read backwards is
	// "tset.tegrat_ym.", and the underscore has to carry a backslash *before*
	// it. Escaping first would produce "_\" reversed, which puts the backslash
	// on the character after.
	const want = `tset.tegrat\_ym.%`
	if pattern != want {
		t.Errorf("pattern = %q, want %q", pattern, want)
	}
	if !strings.HasSuffix(pattern, "%") {
		t.Error("the pattern does not end in a wildcard, so the suffix became an equality")
	}
	if !strings.Contains(compiled.SQL, "reverse("+Alias+".key) LIKE") {
		t.Errorf("the suffix does not go through the reversed index: %s", compiled.SQL)
	}
}

// The search field of the console types into a substring, and a value carrying
// a wildcard must not widen it.
//
// It is the operator no index answers, so it is granted on the name alone. What
// it must not do on top of that is let "50%" match everything: the escape is
// what keeps a typed character a character.
func TestANameSearchIsASubstringAndNotAPattern(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"contains","field":"key","value":"ad_min%"}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Args[1] != `%ad\_min\%%` {
		t.Errorf("pattern = %v, want the value escaped between two wildcards", compiled.Args[1])
	}
	// Case folded, because a person typing into a search field is not spelling
	// a normalized key.
	if !strings.Contains(compiled.SQL, Alias+".key ILIKE") {
		t.Errorf("the substring is case sensitive: %s", compiled.SQL)
	}
}

// A suffix stays a string suffix rather than a notion of domain membership, and
// the dot in the pattern is what makes "evil-target.test" not come back under
// "target.test". Inventing a domain notion now would freeze scope semantics in
// the place where one is merely searching.
func TestASuffixIsAStringSuffix(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"suffix","field":"key","value":".target.test"}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Args[1] != "tset.tegrat.%" {
		t.Errorf("pattern = %v", compiled.Args[1])
	}
}

// Reversal is by rune, because reverse() in PostgreSQL reverses characters. By
// byte it would produce a pattern matching nothing on any internationalized
// name, and an inventory holds those without anybody noticing.
func TestAReversalIsByCharacterAndNotByByte(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"suffix","field":"key","value":".café.test"}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Args[1] != "tset.éfac.%" {
		t.Errorf("pattern = %q, want the value reversed by character", compiled.Args[1])
	}
}

// TestEveryValueIsAParameter is the rule with no exception.
func TestEveryValueIsAParameter(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"and","clauses":[
		{"op":"eq","field":"key","value":"'; DROP TABLE asset; --"},
		{"op":"contains","field":"technologies","value":"nginx'"},
		{"op":"eq","field":"favicon_hash","value":"\"}]"}
	]}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, fragment := range []string{"DROP", "nginx", "--", "'"} {
		if strings.Contains(compiled.SQL, fragment) {
			t.Errorf("the compiled SQL carries %q from a value: %s", fragment, compiled.SQL)
		}
	}
}

// The JSONB fields compile to containment and to nothing else, which is the one
// form the GIN index serves. An "->>" beside it would be an unindexed full scan
// behind an operator indistinguishable from the indexed one.
func TestTheJSONFieldsCompileToContainment(t *testing.T) {
	t.Parallel()

	for _, tree := range []string{
		`{"op":"eq","field":"favicon_hash","value":"abc"}`,
		`{"op":"contains","field":"cookie_name","value":"SESS"}`,
		`{"op":"contains","field":"script_hash","value":"h"}`,
	} {
		compiled, err := Compile(uuid.New(), parse(t, tree))
		if err != nil {
			t.Fatalf("compile %s: %v", tree, err)
		}
		if !strings.Contains(compiled.SQL, Alias+".attributes @> $2::jsonb") {
			t.Errorf("%s compiles to %q rather than to containment", tree, compiled.SQL)
		}
	}
}

// A tree deep enough to exhaust the stack is refused rather than walked.
func TestADeepTreeIsRefused(t *testing.T) {
	t.Parallel()

	tree := `{"op":"eq","field":"port","value":1}`
	for range maxDepth + 2 {
		tree = `{"op":"not","clauses":[` + tree + `]}`
	}
	if _, err := Parse([]byte(tree)); err == nil {
		t.Fatal("a tree deeper than the bound was accepted")
	}
}

// A malformed identifier is the caller's mistake and has to be named as one.
//
// The first version bound the value and cast it at the placeholder, so a typo in
// a hand-edited filter reached PostgreSQL, failed there, and came back as a 500
// and a line in our log: the caller was told the request could not be served
// when the honest answer names the field.
func TestAMalformedIdentifierIsRefusedRatherThanRunTest(t *testing.T) {
	t.Parallel()

	for name, tree := range map[string]string{
		"a value that is not an identifier": `{"op":"eq","field":"program_id","value":"not-a-uuid"}`,
		"an empty one":                      `{"op":"eq","field":"program_id","value":""}`,
		"one bad member of a list":          `{"op":"in","field":"program_id","value":["` + uuid.New().String() + `","nope"]}`,
	} {
		node, err := Parse([]byte(tree))
		if err != nil {
			// Refused at the parse, which is just as good an answer.
			continue
		}
		if _, err := Compile(uuid.New(), node); err == nil {
			t.Errorf("%s compiled, so it reaches the database to be refused there", name)
		}
	}

	// And the positive control, without which the above passes on a field that
	// refuses everything.
	if _, err := Compile(uuid.New(), parse(t,
		`{"op":"eq","field":"program_id","value":"`+uuid.New().String()+`"}`)); err != nil {
		t.Errorf("a well formed identifier was refused: %v", err)
	}
}

// The third state of a nullable boolean, and the fact that it is the only way
// to ask for it.
//
// is_cdn is null when no pass has been able to look, which ingestion writes
// deliberately: a resolution that timed out carries no address, no CNAME and no
// provider, and writing false from it would clear the flag on an asset that is
// genuinely behind an edge. The column therefore answers three things, and
// equality reaches two of them.
//
// The negation does not reach the third and cannot. In SQL NOT (NULL = true) is
// NULL, and a null predicate excludes the row rather than returning it, so
// "everything that is not fronted" written as a negation silently drops every
// asset nobody has looked at.
func TestANullableBooleanCanBeAskedAboutItsThirdState(t *testing.T) {
	t.Parallel()

	org := uuid.New()
	for name, expect := range map[string]struct {
		tree string
		sql  string
	}{
		"present": {`{"op":"exists","field":"is_cdn","value":true}`, "c.is_cdn IS NOT NULL"},
		"absent":  {`{"op":"exists","field":"is_cdn","value":false}`, "c.is_cdn IS NULL"},
		"the other one": {
			`{"op":"exists","field":"waf_detected","value":false}`, "c.waf_detected IS NULL",
		},
	} {
		compiled, err := Compile(org, parse(t, expect.tree))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(compiled.SQL, expect.sql) {
			t.Errorf("%s compiles to %q, want it to carry %q", name, compiled.SQL, expect.sql)
		}
		// A null test is not a bound value, and binding one would be a
		// placeholder compared against NULL, which is never true.
		if len(compiled.Args) != 1 {
			t.Errorf("%s binds %v beside the organization", name, compiled.Args[1:])
		}
	}
}

// The value is not decoration on this operator. It decides the direction, so a
// node without one is a question nobody asked rather than a default.
func TestExistsRefusesANodeWithNoValue(t *testing.T) {
	t.Parallel()

	for name, tree := range map[string]string{
		"on a column":   `{"op":"exists","field":"is_cdn"}`,
		"on a json key": `{"op":"exists","field":"takeover_candidate"}`,
	} {
		if _, err := Compile(uuid.New(), parse(t, tree)); err == nil {
			t.Errorf("%s compiled with no value, so the direction was chosen for the caller", name)
		}
	}
}

// Equality still compiles to a bound comparison. Without this the branch above
// could swallow the ordinary case and nothing would say so.
func TestABooleanEqualityIsStillABoundComparison(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"eq","field":"is_cdn","value":false}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(compiled.SQL, "c.is_cdn = $2") {
		t.Errorf("an equality compiles to %q", compiled.SQL)
	}
	if len(compiled.Args) != 2 || compiled.Args[1] != false {
		t.Errorf("it binds %v", compiled.Args)
	}
}

// A group nested in a group is parenthesised, and this asserts the SQL rather
// than the fact that it compiles.
//
// SQL binds AND tighter than OR and the tree does not. Joined flat,
// "and(a, or(b, c))" is "a AND b OR c", which PostgreSQL reads as
// "(a AND b) OR c": the OR branch escapes every condition beside it. "Port 443,
// and 200 or 301" then answers with everything carrying a 301 on any port.
//
// The reason this went unnoticed is worth keeping beside the fix. Nothing here
// asserted the shape of the SQL, only that a tree compiled and that the
// organization was bound first, and the one deep tree in the suite has a single
// clause in its nested group, which is exactly the case where flat and
// parenthesised are the same string.
func TestANestedGroupIsParenthesised(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"and","clauses":[
		{"op":"eq","field":"port","value":443},
		{"op":"or","clauses":[
			{"op":"eq","field":"status_code","value":200},
			{"op":"eq","field":"status_code","value":301}]}]}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	const want = "c.port = $2 AND (c.status_code = $3 OR c.status_code = $4)"
	if !strings.Contains(compiled.SQL, want) {
		t.Errorf("compiles to\n  %s\nwant it to carry\n  %s", compiled.SQL, want)
	}
}

// The organization is not among the conditions an unparenthesised OR would
// escape, and that is a fact of where the clause is joined rather than luck.
// Compile wraps the whole tree before putting the tenant beside it, so the
// widening above is within a tenant and never across two.
//
// Asserted on the shape that would have exposed it, which is an OR at the root:
// a flat join there would have produced "org_id = $1 AND a OR b", and the OR
// branch would have carried rows belonging to somebody else.
func TestTheOrganizationSurvivesAnOrAtTheRoot(t *testing.T) {
	t.Parallel()

	compiled, err := Compile(uuid.New(), parse(t, `{"op":"or","clauses":[
		{"op":"eq","field":"port","value":443},
		{"op":"eq","field":"port","value":80}]}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	const want = "c.org_id = $1 AND ((c.port = $2 OR c.port = $3))"
	if compiled.SQL != want {
		t.Errorf("compiles to\n  %s\nwant\n  %s", compiled.SQL, want)
	}
}
