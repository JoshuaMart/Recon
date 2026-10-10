package search

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Compiled is a WHERE fragment and the values it binds.
type Compiled struct {
	SQL  string
	Args []any
}

// builder accumulates the parameters as the tree is walked.
type builder struct{ args []any }

// bind adds a value and returns its placeholder.
func (b *builder) bind(value any) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

// Compile turns a tree into SQL for one organization. The organization is emitted here,
// outside the tree, on every compilation including an empty one: a clause the caller can
// express is one the caller can forget, and this query is not in the static query files, so
// the tenant guard does not see it. Defence in order of strength: this structural clause
// (required by a test), then row-level security, which the compiler cannot remove.
func Compile(org uuid.UUID, node Node) (Compiled, error) {
	if org == uuid.Nil {
		return Compiled{}, refuse("a query was compiled with no organization")
	}

	b := &builder{}
	tenant := Alias + ".org_id = " + b.bind(org)

	clause, err := b.node(node, 0)
	if err != nil {
		return Compiled{}, err
	}
	if clause == "" {
		return Compiled{SQL: tenant, Args: b.args}, nil
	}
	return Compiled{SQL: tenant + " AND (" + clause + ")", Args: b.args}, nil
}

func (b *builder) node(n Node, depth int) (string, error) {
	switch n.Op {
	// The zero value, passed by a caller filtering on nothing. Reaching the leaf
	// path would refuse the most common request with a message about an empty
	// operator.
	case "":
		if depth > 0 {
			return "", refuse("a clause with no operator")
		}
		return "", nil

	case OpAnd, OpOr:
		return b.group(n, strings.ToUpper(n.Op), depth)

	case OpNot:
		inner, err := b.group(n, "AND", depth)
		if err != nil {
			return "", err
		}
		if inner == "" {
			// Unreachable while a nested empty group is refused. Kept so relaxing that rule
			// cannot turn a negation into its opposite: an empty group means "no
			// constraint", and dropping the NOT would answer the whole inventory instead of
			// nothing.
			return "", refuse("a negation with nothing to negate")
		}
		return "NOT (" + inner + ")", nil

	default:
		return b.leaf(n)
	}
}

func (b *builder) group(n Node, joiner string, depth int) (string, error) {
	parts := make([]string, 0, len(n.Clauses))
	for _, clause := range n.Clauses {
		compiled, err := b.node(clause, depth+1)
		if err != nil {
			return "", err
		}
		parts = append(parts, compiled)
	}

	if len(parts) == 0 {
		// At the root an empty group is not an error: it is what an interface sends
		// before any facet is clicked, and the whole tenant is right.
		//
		// Elsewhere it is refused, because every reading is a guess. An empty AND is
		// TRUE and an empty OR is FALSE, so "or(port=443, and())" means everything and
		// "and(port=443, or())" nothing, while a console that cleared a group's last
		// facet meant neither. Refusing names the group.
		if depth > 0 {
			return "", refuse("a %q group with nothing in it", n.Op)
		}
		return "", nil
	}
	// Parenthesised, because SQL binds AND tighter than OR and the tree does not.
	// Flat, "and(a, or(b, c))" is "(a AND b) OR c", and the OR branch escapes every
	// condition beside it.
	//
	// The failure is silent and widens: "port 443, and 200 or 301" returns every
	// 301 on any port, which looks like a result. It widens within a tenant only,
	// as Compile wraps the tree before joining the tenant clause.
	joined := strings.Join(parts, " "+joiner+" ")
	if len(parts) > 1 {
		joined = "(" + joined + ")"
	}
	return joined, nil
}

func (b *builder) leaf(n Node) (string, error) {
	entry := registry[n.Field]

	switch entry.kind {
	case kindJSONText, kindJSONArray:
		return b.containment(entry, n)
	case kindJSONExists:
		return b.exists(entry, n)
	case kindTextArray:
		return b.array(entry, n)
	case kindInet:
		return b.address(entry, n)
	case kindUUID:
		return b.identifier(entry, n)
	case kindTimestamp:
		return b.instant(entry, n)
	case kindBool:
		value, ok := n.Value.(bool)
		if !ok {
			return "", refuse("%q takes true or false", n.Field)
		}
		// A nullable boolean has three states, and exists asks about the third ("is
		// there a value"), as over the attributes object.
		//
		// It is also the only way to ask: NOT (NULL = true) is NULL and excludes the
		// row, so the state and its negation are otherwise unexpressible.
		if n.Op == OpExists {
			if value {
				return entry.expr + " IS NOT NULL", nil
			}
			return entry.expr + " IS NULL", nil
		}
		return entry.expr + " = " + b.bind(value), nil
	case kindInt:
		return b.number(entry, n)
	default:
		return b.text(entry, n)
	}
}

// containment is the only shape the JSONB fields compile to. The object is built
// in Go and bound as one parameter, so a caller's value is never a fragment. It
// covers scalar and array keys, which the GIN index answers.
func (b *builder) containment(entry field, n Node) (string, error) {
	value, ok := n.Value.(string)
	if !ok || value == "" {
		return "", refuse("%q takes a non-empty string", n.Field)
	}

	var object map[string]any
	if entry.kind == kindJSONArray {
		object = map[string]any{entry.key: []string{value}}
	} else {
		object = map[string]any{entry.key: value}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", refuse("%q could not be encoded", n.Field)
	}
	return Alias + ".attributes @> " + b.bind(encoded) + "::jsonb", nil
}

func (b *builder) exists(entry field, n Node) (string, error) {
	present, ok := n.Value.(bool)
	if !ok {
		return "", refuse("%q takes true or false", n.Field)
	}
	clause := Alias + ".attributes ? " + b.bind(entry.key)
	if !present {
		return "NOT (" + clause + ")", nil
	}
	return clause, nil
}

func (b *builder) array(entry field, n Node) (string, error) {
	switch n.Op {
	case OpContains:
		value, ok := n.Value.(string)
		if !ok || value == "" {
			return "", refuse("%q takes a non-empty string", n.Field)
		}
		// Containment, not ANY: it is what the GIN index answers.
		return entry.expr + " @> ARRAY[" + b.bind(value) + "]::text[]", nil

	case OpIn:
		values, err := stringList(n)
		if err != nil {
			return "", err
		}
		return entry.expr + " && " + b.bind(values) + "::text[]", nil

	default:
		return "", refuse("%q does not accept %q", n.Field, n.Op)
	}
}

func (b *builder) address(entry field, n Node) (string, error) {
	value, ok := n.Value.(string)
	if !ok {
		return "", refuse("%q takes an address", n.Field)
	}

	switch n.Op {
	case OpEq:
		parsed, err := netip.ParseAddr(value)
		if err != nil {
			return "", refuse("%q is not an address", value)
		}
		return entry.expr + " = " + b.bind(parsed.String()) + "::inet", nil

	case OpInCIDR:
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return "", refuse("%q is not a network", value)
		}
		return entry.expr + " <<= " + b.bind(prefix.String()) + "::inet", nil

	default:
		return "", refuse("%q does not accept %q", n.Field, n.Op)
	}
}

func (b *builder) instant(entry field, n Node) (string, error) {
	value, ok := n.Value.(string)
	if !ok {
		return "", refuse("%q takes an RFC 3339 instant", n.Field)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", refuse("%q is not an RFC 3339 instant", value)
	}

	operator := "<"
	if n.Op == OpAfter {
		operator = ">"
	}
	return entry.expr + " " + operator + " " + b.bind(parsed.UTC()), nil
}

func (b *builder) number(entry field, n Node) (string, error) {
	if n.Op == OpIn {
		values, err := numberList(n)
		if err != nil {
			return "", err
		}
		return entry.expr + " = ANY(" + b.bind(values) + "::int[])", nil
	}

	number, err := asInt(n.Value)
	if err != nil {
		return "", refuse("%q takes a number, got %v", n.Field, n.Value)
	}
	return entry.expr + " " + comparison(n.Op) + " " + b.bind(number), nil
}

// identifier compiles an equality or membership over a uuid column. The value
// is checked here and also bound and cast: the cast alone let a malformed id
// reach PostgreSQL, fail with 22P02 and return a 500 for a hand-edited typo. A
// refusal names the field.
func (b *builder) identifier(entry field, n Node) (string, error) {
	switch n.Op {
	case OpIn:
		values, err := stringList(n)
		if err != nil {
			return "", err
		}
		for _, value := range values {
			if _, err := uuid.Parse(value); err != nil {
				return "", refuse("%q takes identifiers, and %q is not one", n.Field, value)
			}
		}
		return entry.expr + " = ANY(" + b.bind(values) + "::uuid[])", nil
	case OpEq:
		value, ok := n.Value.(string)
		if !ok || value == "" {
			return "", refuse("%q takes an identifier", n.Field)
		}
		if _, err := uuid.Parse(value); err != nil {
			return "", refuse("%q takes an identifier, and %q is not one", n.Field, value)
		}
		return entry.expr + " = " + b.bind(value) + "::uuid", nil
	default:
		return "", refuse("%q does not take %q", n.Field, n.Op)
	}
}

func (b *builder) text(entry field, n Node) (string, error) {
	switch n.Op {
	case OpIn:
		values, err := stringList(n)
		if err != nil {
			return "", err
		}
		return entry.expr + " = ANY(" + b.bind(values) + "::text[])", nil

	case OpEq:
		value, ok := n.Value.(string)
		if !ok {
			return "", refuse("%q takes a string", n.Field)
		}
		return entry.expr + " = " + b.bind(value), nil

	case OpPrefix:
		value, ok := n.Value.(string)
		if !ok || value == "" {
			return "", refuse("%q takes a non-empty string", n.Field)
		}
		return entry.expr + " LIKE " + b.bind(escapeLike(value)+"%"), nil

	case OpSuffix:
		value, ok := n.Value.(string)
		if !ok || value == "" {
			return "", refuse("%q takes a non-empty string", n.Field)
		}
		// text_pattern_ops accelerates a prefix, but an ASM query is a suffix (".target.com"), which
		// LIKE '%.target.com' cannot index; the expression index on reverse(key) turns one into the
		// other. Escape after reversing: escaping first puts each backslash after the character it
		// protected, so "_" stays a wildcard and a trailing backslash escapes the appended "%",
		// silently turning the search into an equality (no test notices without a wildcard value).
		return "reverse(" + entry.expr + ") LIKE " + b.bind(escapeLike(reverse(value))+"%"), nil

	case OpContains:
		value, ok := n.Value.(string)
		if !ok || value == "" {
			return "", refuse("%q takes a non-empty string", n.Field)
		}
		// A leading wildcard, so no index answers it; the registry says so where it
		// grants the operator. ILIKE because the column holds a normalized name and
		// "API" asks what "api" does.
		return entry.expr + " ILIKE " + b.bind("%"+escapeLike(value)+"%"), nil

	default:
		return "", refuse("%q does not accept %q", n.Field, n.Op)
	}
}

func comparison(op string) string {
	switch op {
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	default:
		return "="
	}
}

// escapeLike protects the two wildcards and the escape character itself.
func escapeLike(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\\', '%', '_':
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// reverse mirrors a string by rune, as PostgreSQL's reverse() works on
// characters. Reversing bytes would match nothing on any non-ASCII name, such as
// an internationalized domain.
func reverse(value string) string {
	runes := []rune(value)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func asInt(value any) (int, error) {
	switch typed := value.(type) {
	case float64:
		// Through float64: the tree arrives as JSON, where every number is one.
		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("not a whole number")
		}
		return int(typed), nil
	case int:
		return typed, nil
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func stringList(n Node) ([]string, error) {
	items, ok := n.Value.([]any)
	if !ok || len(items) == 0 {
		return nil, refuse("%q takes a non-empty list", n.Field)
	}
	if len(items) > maxClauses {
		return nil, refuse("%q was given %d values, and the bound is %d", n.Field, len(items), maxClauses)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, refuse("%q takes a list of strings", n.Field)
		}
		out = append(out, value)
	}
	return out, nil
}

func numberList(n Node) ([]int32, error) {
	items, ok := n.Value.([]any)
	if !ok || len(items) == 0 {
		return nil, refuse("%q takes a non-empty list", n.Field)
	}
	if len(items) > maxClauses {
		return nil, refuse("%q was given %d values, and the bound is %d", n.Field, len(items), maxClauses)
	}
	out := make([]int32, 0, len(items))
	for _, item := range items {
		number, err := asInt(item)
		if err != nil {
			return nil, refuse("%q takes a list of numbers", n.Field)
		}
		out = append(out, int32(number)) //nolint:gosec // a port or a status code
	}
	return out, nil
}
