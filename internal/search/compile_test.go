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

// The assertion the compiler rests on. This query is not in the static query files,
// so the tenant guard does not see it; the clause is structural, and this asserts it
// on every tree shape, including the empty one an interface sends before any facet
// is clicked.
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

// The other half: an organization filter the caller can express is one they can
// forget, or set to someone else's.
func TestTheTenantCannotBeExpressed(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"org_id", "org", "tenant", "organization"} {
		_, err := Parse([]byte(`{"op":"eq","field":"` + name + `","value":"x"}`))
		if err == nil {
			t.Errorf("%q is a field the registry accepts", name)
		}
	}
}

// A programme is not the tenant under another name. The compiler emits the
// organization on every compilation, so a query can neither name nor omit it. A
// programme is a perimeter inside one organization that the switcher filters on,
// and the tenant clause is still emitted beside it: naming another's programme
// returns nothing, not their inventory.
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

// An empty AND is TRUE and an empty OR is FALSE, so one shape means everything in one
// parent and nothing in the other. The deciding case is negation: "not(and())" is
// "not everything", zero rows, and a compiler letting an empty group contribute
// nothing would answer the whole inventory to a console that cleared the last facet
// of a negated group.
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

// The trap the doc names, invisible without a wildcard in the value. Escaping LIKE
// wildcards before reversing puts each backslash after the character it should
// protect: "_" stays live and a trailing backslash escapes the appended "%", turning
// the search into an equality.
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

	// Reversed first, then escaped: backwards the value is "tset.tegrat_ym.", and the
	// underscore needs the backslash before it. Escaping first would put it after.
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

// The console's search field types a substring, and a wildcard in the value must not
// widen it. No index answers it, so it is granted on the name alone; "50%" must not
// match everything.
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

// A suffix stays a string suffix, not domain membership: the dot makes
// "evil-target.test" not match "target.test". A domain notion here would freeze scope
// semantics in a place that only searches.
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

// Reversal is by rune, as PostgreSQL's reverse() works on characters; by byte it
// would match nothing on internationalized names.
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

// The JSONB fields compile to containment only, the form the GIN index serves; "->>"
// would be an unindexed scan behind an operator that looks indexed.
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

// A malformed identifier is the caller's mistake and is named as one. Binding and
// casting at the placeholder let a hand-edited typo reach PostgreSQL, fail, and
// return a 500 where the honest answer names the field.
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

// The third state of a nullable boolean, and the only way to ask for it. is_cdn is
// null when no pass could look (a timed-out resolution carries no address, CNAME or
// provider, and false would clear the flag on a genuinely fronted asset), so the
// column answers three things and equality reaches two.
//
// Negation does not reach the third: NOT (NULL = true) is NULL, which excludes the
// row, so "everything not fronted" silently drops every asset nobody has looked at.
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

// A group nested in a group is parenthesised; this asserts the SQL, not that it compiles. SQL
// binds AND tighter than OR and the tree does not: flat, "and(a, or(b, c))" is
// "(a AND b) OR c" and the OR branch escapes every condition. It went unnoticed because
// nothing asserted the SQL's shape, and the suite's one deep tree has a single clause in its
// nested group, where flat and parenthesised are identical.
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

// The organization is not among the conditions an unparenthesised OR would escape,
// by where the clause is joined: Compile wraps the whole tree before adding the
// tenant, so any widening stays within a tenant.
//
// Asserted on the shape that would expose it, an OR at the root: a flat join gives
// "org_id = $1 AND a OR b", and the OR branch would carry another tenant's rows.
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
