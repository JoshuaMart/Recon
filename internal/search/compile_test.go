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
