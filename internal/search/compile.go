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

// Compile turns a tree into SQL for one organization.
//
// The organization is emitted here, outside the tree, on every compilation
// including that of an empty one. It is not a clause the caller can express,
// because a filter the caller can express is a filter the caller can forget,
// and this query does not live in the static query files so the tenant guard
// does not see it. What holds instead, in order of strength: this clause is
// structural and a test requires it on every compilation; then row-level
// security, which is the only guarantee the compiler cannot remove from itself.
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
	// The zero value, which is what a caller that filters on nothing passes.
	// Reaching the leaf path with it would refuse the most common request in
	// the system, and refuse it with a message about an empty operator.
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
			// Unreachable while a nested empty group is a refusal, and kept so
			// that relaxing that rule cannot quietly turn a negation into its
			// opposite: an empty group means "no constraint", which is true,
			// and dropping the NOT around it would answer the whole inventory
			// where the honest answer is nothing at all.
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
		// At the root an empty group is not an error: it is what an interface
		// sends before anybody has clicked a facet, and the whole tenant is the
		// right answer.
		//
		// Anywhere else it is a refusal, because there is no reading of it that
		// is not a guess. An empty AND is TRUE by identity and an empty OR is
		// FALSE, so "or(port=443, and())" means the whole inventory while
		// "and(port=443, or())" means nothing at all, and a console that
		// cleared the last facet out of a group meant neither. Refusing names
		// the group; picking one silently answers a different question.
		if depth > 0 {
			return "", refuse("a %q group with nothing in it", n.Op)
		}
		return "", nil
	}
	// Parenthesised, because SQL binds AND tighter than OR and the tree does
	// not. Joined flat, "and(a, or(b, c))" is "a AND b OR c", which PostgreSQL
	// reads as "(a AND b) OR c": the OR branch escapes every condition beside
	// it and answers a question nobody asked.
	//
	// The failure is silent and it widens. "port 443, and 200 or 301" comes
	// back as everything carrying a 301 on any port, which looks like a result
	// rather than like an error, and the only way to notice is to read the SQL.
	// The organization is not among the conditions it escapes: Compile wraps
	// the whole tree before joining the tenant clause to it, so this widens
	// within a tenant and never across two.
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
		// A nullable boolean has three states and exists is what asks about the
		// third. It reads the same as it does over the attributes object, "is
		// there a value for this", against a column instead of a key.
		//
		// It is also the only way to ask. A negation does not reach it, because
		// NOT (NULL = true) is NULL and a null predicate excludes the row: the
		// state and its negation are both unexpressible without this.
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

// containment is the only shape the JSONB fields compile to.
//
// The object is built in Go and bound as one parameter, so a value chosen by
// the caller is a value and never a fragment. It covers a scalar key and an
// array key alike, which is what the GIN index answers.
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
		// Containment rather than ANY, because that is what the GIN index on
		// the column answers.
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

// identifier compiles an equality or a membership over a uuid column.
//
// The value is checked here and still bound and cast at the placeholder. The
// cast alone was the first version and it is not enough: a malformed identifier
// reaches PostgreSQL, fails there with 22P02, and comes back to the caller as a
// 500 and a line in our log, when it is a typo in a filter somebody hand
// edited. A refusal names the field, and a refusal is what the whole registry
// is built to answer with.
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
		// A text_pattern_ops index accelerates a prefix. The query of an ASM
		// inventory is a suffix, ".target.com", and a LIKE '%.target.com' can
		// use no index at all. The expression index on reverse(key) turns one
		// into the other.
		//
		// The escaping happens after reversing, and that order is the whole
		// trap. Escaping first puts each backslash after the character it was
		// meant to protect, so "_" stays a live wildcard and a trailing
		// backslash escapes the "%" appended below: the search silently becomes
		// an equality, and no test sees it unless a value contains a wildcard.
		return "reverse(" + entry.expr + ") LIKE " + b.bind(escapeLike(reverse(value))+"%"), nil

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

// reverse mirrors a string by rune.
//
// By rune rather than by byte, because reverse() in PostgreSQL reverses
// characters. Reversing bytes here would produce a pattern that matches nothing
// on any name carrying a non-ASCII character, and an internationalized domain
// name is exactly the sort of thing an inventory holds without anybody noticing.
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
		// Through float64 because the tree arrives as JSON, where every number
		// is one.
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
