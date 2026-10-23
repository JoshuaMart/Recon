// Package search is the filter language: a parser from text to an AST that
// prints back to text, a field registry, and a compiler from the AST to
// parameterized SQL over asset_current.
package search

import (
	"fmt"
	"strings"
	"unicode"
)

// Node is a query tree. String prints it back as text, canonical form.
type Node interface {
	String() string
}

// And is two nodes that both hold.
type And struct{ Left, Right Node }

// Or is two nodes of which one holds.
type Or struct{ Left, Right Node }

// Not negates a node.
type Not struct{ Node Node }

// Has is has:field.
type Has struct{ Field string }

// Term is field:values, OR between the values.
type Term struct {
	Field  string
	Values []Value
	// Versions is the @ constraint of a tech term, OR between them. Nil means
	// no constraint, NoVersion means @?.
	Versions  []Value
	NoVersion bool
}

// Value is one value of a term.
type Value struct {
	Op     string // "", ">", ">=", "<", "<="
	Lit    string
	To     string // set for a range a..b
	Regex  bool
	Quoted bool
}

func (n And) String() string { return group(n.Left, false) + " " + group(n.Right, false) }
func (n Or) String() string  { return n.Left.String() + " OR " + n.Right.String() }
func (n Not) String() string { return "-" + group(n.Node, true) }
func (n Has) String() string { return "has:" + n.Field }

// group parenthesizes what would bind differently printed bare: an OR inside
// an AND, and anything composite under a NOT.
func group(n Node, underNot bool) string {
	switch n.(type) {
	case Or:
		return "(" + n.String() + ")"
	case And:
		if underNot {
			return "(" + n.String() + ")"
		}
	}
	return n.String()
}

func (t Term) String() string {
	var b strings.Builder
	b.WriteString(t.Field)
	b.WriteByte(':')
	for i, v := range t.Values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(v.String())
	}
	if t.NoVersion {
		b.WriteString("@?")
	}
	for i, v := range t.Versions {
		if i == 0 {
			b.WriteByte('@')
		} else {
			b.WriteByte(',')
		}
		b.WriteString(v.String())
	}
	return b.String()
}

func (v Value) String() string {
	switch {
	case v.Regex:
		return "/" + v.Lit + "/"
	case v.To != "":
		return quote(v.Lit) + ".." + quote(v.To)
	}
	return v.Op + quote(v.Lit)
}

// quote prints a literal, quoted when it holds what would end it.
func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t,()\"@") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

// SyntaxError points at where the query stopped making sense.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("at %d: %s", e.Pos, e.Msg) }

// maxQuery bounds a query's length: it is typed, not generated.
const maxQuery = 4096

// Parse reads a query. An empty query is nil: everything.
func Parse(q string) (Node, error) {
	if len(q) > maxQuery {
		return nil, &SyntaxError{0, "query longer than 4096 characters"}
	}
	p := &parser{src: q}
	p.skip()
	if p.done() {
		return nil, nil
	}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	p.skip()
	if !p.done() {
		return nil, p.errf("unexpected %q", p.src[p.pos:])
	}
	return n, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) done() bool { return p.pos >= len(p.src) }
func (p *parser) peek() byte {
	if p.done() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skip() {
	for !p.done() && unicode.IsSpace(rune(p.src[p.pos])) {
		p.pos++
	}
}

func (p *parser) errf(format string, a ...any) error {
	return &SyntaxError{p.pos, fmt.Sprintf(format, a...)}
}

// keyword reports whether kw (OR, NOT) stands at the cursor as a word.
func (p *parser) keyword(kw string) bool {
	if !strings.HasPrefix(p.src[p.pos:], kw) {
		return false
	}
	end := p.pos + len(kw)
	return end == len(p.src) || unicode.IsSpace(rune(p.src[end])) || p.src[end] == '('
}

func (p *parser) or() (Node, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for {
		p.skip()
		if !p.keyword("OR") {
			return left, nil
		}
		p.pos += 2
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = Or{left, right}
	}
}

func (p *parser) and() (Node, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		p.skip()
		if p.done() || p.peek() == ')' || p.keyword("OR") {
			return left, nil
		}
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		left = And{left, right}
	}
}

func (p *parser) unary() (Node, error) {
	p.skip()
	switch {
	case p.peek() == '-':
		p.pos++
		n, err := p.atom()
		return Not{n}, err
	case p.keyword("NOT"):
		p.pos += 3
		n, err := p.atom()
		return Not{n}, err
	}
	return p.atom()
}

func (p *parser) atom() (Node, error) {
	p.skip()
	if p.done() {
		return nil, p.errf("a term is missing")
	}
	if p.peek() == '(' {
		p.pos++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.peek() != ')' {
			return nil, p.errf("a ) is missing")
		}
		p.pos++
		return n, nil
	}
	return p.term()
}

func (p *parser) term() (Node, error) {
	start := p.pos
	for !p.done() && p.peek() != ':' && !unicode.IsSpace(rune(p.peek())) && p.peek() != ')' {
		p.pos++
	}
	field := strings.ToLower(p.src[start:p.pos])
	if p.peek() != ':' || field == "" {
		return nil, &SyntaxError{start, fmt.Sprintf("%q is not field:value", p.src[start:p.pos])}
	}
	p.pos++
	if field == "has" {
		vs, err := p.values(false)
		if err != nil {
			return nil, err
		}
		if len(vs) != 1 || vs[0].Op != "" || vs[0].To != "" || vs[0].Regex {
			return nil, &SyntaxError{start, "has: takes one field"}
		}
		return Has{Field: strings.ToLower(vs[0].Lit)}, nil
	}
	t := Term{Field: field}
	vs, err := p.values(field == "tech")
	if err != nil {
		return nil, err
	}
	t.Values = vs
	if field == "tech" && p.peek() == '@' {
		p.pos++
		if p.peek() == '?' {
			p.pos++
			t.NoVersion = true
		} else if t.Versions, err = p.values(false); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// values reads a comma separated list. atStops is set for tech, where @ ends
// the name.
func (p *parser) values(atStops bool) ([]Value, error) {
	var out []Value
	for {
		v, err := p.value(atStops)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.peek() != ',' {
			return out, nil
		}
		p.pos++
	}
}

func (p *parser) value(atStops bool) (Value, error) {
	var v Value
	if p.peek() == '/' {
		end := strings.IndexByte(p.src[p.pos+1:], '/')
		if end < 0 {
			return v, p.errf("a regex is not closed")
		}
		v.Lit, v.Regex = p.src[p.pos+1:p.pos+1+end], true
		p.pos += end + 2
		return v, nil
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(p.src[p.pos:], op) {
			v.Op = op
			p.pos += len(op)
			break
		}
	}
	lit, quoted, err := p.literal(atStops)
	if err != nil {
		return v, err
	}
	v.Lit, v.Quoted = lit, quoted
	if v.Op == "" && !quoted {
		if a, b, ok := strings.Cut(lit, ".."); ok && a != "" && b != "" {
			v.Lit, v.To = a, b
		}
	}
	if !quoted && strings.HasPrefix(p.src[p.pos:], "..") {
		p.pos += 2
		to, _, err := p.literal(atStops)
		if err != nil {
			return v, err
		}
		v.To = to
	}
	if v.Lit == "" && !quoted {
		return v, p.errf("a value is missing")
	}
	return v, nil
}

func (p *parser) literal(atStops bool) (string, bool, error) {
	if p.peek() == '"' {
		p.pos++
		var b strings.Builder
		for !p.done() {
			c := p.peek()
			switch {
			case c == '\\' && p.pos+1 < len(p.src):
				b.WriteByte(p.src[p.pos+1])
				p.pos += 2
				continue
			case c == '"':
				p.pos++
				return b.String(), true, nil
			}
			b.WriteByte(c)
			p.pos++
		}
		return "", false, p.errf("a quote is not closed")
	}
	start := p.pos
	for !p.done() {
		c := p.peek()
		if unicode.IsSpace(rune(c)) || c == ',' || c == ')' || c == '(' || (atStops && c == '@') {
			break
		}
		p.pos++
	}
	return p.src[start:p.pos], false, nil
}
