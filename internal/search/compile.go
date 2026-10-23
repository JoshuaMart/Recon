package search

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JoshuaMart/recon/internal/fingerprint"
)

// From is what a compiled query selects from. The compiler only emits the
// WHERE condition over these aliases.
const From = `asset_current ac
JOIN program p ON p.id = ac.program_id
JOIN asset a ON a.id = ac.asset_id
LEFT JOIN triage tr ON tr.asset_id = ac.asset_id`

// QueryError is a query that parses and asks for something the language does
// not have.
type QueryError struct {
	Kind    string // unknown_field, bad_value
	Field   string
	Detail  string
	Suggest string
}

func (e *QueryError) Error() string { return e.Detail }

// Compiled is a condition and its bind parameters, numbered from $1.
type Compiled struct {
	Where string
	Args  []any
}

// Compile turns a tree into a parameterized condition. A nil tree is TRUE.
// Every value is a bind parameter: nothing from the query reaches the SQL
// text but field names, which come from the registry.
func Compile(n Node, now time.Time) (Compiled, error) {
	c := &compiler{now: now}
	if n == nil {
		return Compiled{Where: "TRUE"}, nil
	}
	w, err := c.node(n)
	if err != nil {
		return Compiled{}, err
	}
	return Compiled{Where: w, Args: c.args}, nil
}

// MentionsKind reports whether a query filters on kind itself, in which case
// a view does not narrow it.
func MentionsKind(n Node) bool {
	switch v := n.(type) {
	case And:
		return MentionsKind(v.Left) || MentionsKind(v.Right)
	case Or:
		return MentionsKind(v.Left) || MentionsKind(v.Right)
	case Not:
		return MentionsKind(v.Node)
	case Term:
		return v.Field == "kind"
	}
	return false
}

type compiler struct {
	now  time.Time
	args []any
}

func (c *compiler) arg(v any) string {
	c.args = append(c.args, v)
	return "$" + strconv.Itoa(len(c.args))
}

func (c *compiler) node(n Node) (string, error) {
	switch v := n.(type) {
	case And:
		l, err := c.node(v.Left)
		if err != nil {
			return "", err
		}
		r, err := c.node(v.Right)
		return "(" + l + " AND " + r + ")", err
	case Or:
		l, err := c.node(v.Left)
		if err != nil {
			return "", err
		}
		r, err := c.node(v.Right)
		return "(" + l + " OR " + r + ")", err
	case Not:
		inner, err := c.node(v.Node)
		return "NOT " + inner, err
	case Has:
		cond, ok := hasFields[v.Field]
		if !ok {
			names := make([]string, 0, len(hasFields))
			for h := range hasFields {
				names = append(names, h)
			}
			return "", &QueryError{Kind: "unknown_field", Field: "has:" + v.Field, Detail: fmt.Sprintf("has: does not know %q", v.Field), Suggest: suggest(v.Field, names)}
		}
		return "COALESCE((" + cond + "), false)", nil
	case Term:
		cond, err := c.term(v)
		// A term is false, never unknown, so NOT keeps what has no value.
		return "COALESCE((" + cond + "), false)", err
	}
	return "", fmt.Errorf("search: unknown node %T", n)
}

func (c *compiler) term(t Term) (string, error) {
	if name, ok := strings.CutPrefix(t.Field, "header."); ok && name != "" {
		return c.anyOf(t, func(v Value) (string, error) {
			return c.text("ac.headers ->> "+c.arg(strings.ToLower(name)), t.Field, v)
		})
	}
	f, ok := fields[t.Field]
	if !ok {
		names := make([]string, 0, len(fields))
		for n := range fields {
			names = append(names, n)
		}
		return "", &QueryError{Kind: "unknown_field", Field: t.Field, Detail: fmt.Sprintf("unknown field %q", t.Field), Suggest: suggest(t.Field, names)}
	}
	if (t.NoVersion || len(t.Versions) > 0) && f.Kind != KTech {
		return "", c.bad(t.Field, "only tech takes a version")
	}
	switch f.Kind {
	case KText:
		col := f.column
		if t.Field == "program" {
			return c.anyOf(t, func(v Value) (string, error) {
				a, err := c.text("p.name", t.Field, v)
				if err != nil {
					return "", err
				}
				b, err := c.text("p.slug", t.Field, v)
				return "(" + a + " OR " + b + ")", err
			})
		}
		return c.anyOf(t, func(v Value) (string, error) { return c.text(col, t.Field, v) })
	case KDomain:
		return c.anyOf(t, func(v Value) (string, error) { return c.domain(f.column, t.Field, v) })
	case KEnum:
		return c.enum(f, t)
	case KNumber, KStatus:
		return c.anyOf(t, func(v Value) (string, error) { return c.number(f, t.Field, v) })
	case KIP:
		return c.anyOf(t, func(v Value) (string, error) { return c.ip(f.column, t.Field, v) })
	case KArray, KHeader:
		return c.anyOf(t, func(v Value) (string, error) {
			m, err := c.text("e", t.Field, v)
			return "EXISTS (SELECT 1 FROM unnest(" + f.column + ") e WHERE " + m + ")", err
		})
	case KDate:
		return c.anyOf(t, func(v Value) (string, error) { return c.date(f.column, t.Field, v) })
	case KTech:
		return c.tech(t)
	case KCategory:
		return c.anyOf(t, func(v Value) (string, error) {
			m, err := c.text("tc.category", t.Field, v)
			return "EXISTS (SELECT 1 FROM asset_tech tc WHERE tc.asset_id = ac.asset_id AND " + m + ")", err
		})
	}
	return "", fmt.Errorf("search: field %q has no compiler", t.Field)
}

func (c *compiler) bad(field, format string, a ...any) error {
	return &QueryError{Kind: "bad_value", Field: field, Detail: field + ": " + fmt.Sprintf(format, a...)}
}

// anyOf ORs a term's values.
func (c *compiler) anyOf(t Term, one func(Value) (string, error)) (string, error) {
	parts := make([]string, 0, len(t.Values))
	for _, v := range t.Values {
		p, err := one(v)
		if err != nil {
			return "", err
		}
		parts = append(parts, p)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

const maxRegex = 200

// text is equality without case, a glob with *, or a regex.
func (c *compiler) text(col, field string, v Value) (string, error) {
	switch {
	case v.Op != "" || v.To != "":
		return "", c.bad(field, "a text field takes no comparison or range")
	case v.Regex:
		if len(v.Lit) > maxRegex {
			return "", c.bad(field, "a regex longer than %d characters", maxRegex)
		}
		if _, err := regexp.Compile(v.Lit); err != nil {
			return "", c.bad(field, "bad regex: %v", err)
		}
		return col + " ~* " + c.arg(v.Lit), nil
	case strings.Contains(v.Lit, "*"):
		return col + " ILIKE " + c.arg(likeGlob(v.Lit)) + ` ESCAPE '\'`, nil
	}
	return "lower(" + col + ") = lower(" + c.arg(v.Lit) + ")", nil
}

// likeGlob turns a * glob into a LIKE pattern, escaping LIKE's own wildcards.
func likeGlob(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`)
	return r.Replace(s)
}

func (c *compiler) domain(col, field string, v Value) (string, error) {
	if v.Op != "" || v.To != "" || v.Regex || strings.Contains(v.Lit, "*") {
		return "", c.bad(field, "takes a domain, without wildcard")
	}
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v.Lit)), ".")
	// Label boundaries: target.com matches a.target.com, never eviltarget.com.
	return "(" + col + " = " + c.arg(d) + " OR " + col + " LIKE " + c.arg("%."+likeGlob(d)) + ` ESCAPE '\')`, nil
}

var scopeValues = map[string]string{
	"in": "in_scope", "out": "out_of_scope", "unknown": "unknown",
	"in_scope": "in_scope", "out_of_scope": "out_of_scope",
}

func (c *compiler) enum(f Field, t Term) (string, error) {
	var vals []string
	for _, v := range t.Values {
		if v.Op != "" || v.To != "" || v.Regex {
			return "", c.bad(t.Field, "takes values of %v", f.Values)
		}
		lit := strings.ToLower(v.Lit)
		if t.Field == "scope" {
			mapped, ok := scopeValues[lit]
			if !ok {
				return "", c.bad(t.Field, "%q is not one of in, out, unknown", v.Lit)
			}
			vals = append(vals, mapped)
			continue
		}
		if !slices.Contains(f.Values, lit) {
			return "", c.bad(t.Field, "%q is not one of %v", v.Lit, f.Values)
		}
		vals = append(vals, lit)
	}
	return f.column + " = ANY(" + c.arg(vals) + "::text[])", nil
}

func (c *compiler) number(f Field, field string, v Value) (string, error) {
	if v.Regex {
		return "", c.bad(field, "a number takes no regex")
	}
	if f.Kind == KStatus && v.Op == "" && v.To == "" && len(v.Lit) == 3 && strings.HasSuffix(strings.ToLower(v.Lit), "xx") {
		d, err := strconv.Atoi(v.Lit[:1])
		if err != nil || d < 1 || d > 5 {
			return "", c.bad(field, "%q is not a status class", v.Lit)
		}
		return f.column + " BETWEEN " + c.arg(d*100) + " AND " + c.arg(d*100+99), nil
	}
	lo, err := strconv.Atoi(v.Lit)
	if err != nil {
		return "", c.bad(field, "%q is not a number", v.Lit)
	}
	if v.To != "" {
		hi, err := strconv.Atoi(v.To)
		if err != nil {
			return "", c.bad(field, "%q is not a number", v.To)
		}
		return f.column + " BETWEEN " + c.arg(lo) + " AND " + c.arg(hi), nil
	}
	op := v.Op
	if op == "" {
		op = "="
	}
	return f.column + " " + op + " " + c.arg(lo), nil
}

func (c *compiler) ip(col, field string, v Value) (string, error) {
	if v.Op != "" || v.To != "" || v.Regex {
		return "", c.bad(field, "takes an address or a network")
	}
	p, err := netip.ParsePrefix(v.Lit)
	if err != nil {
		a, aerr := netip.ParseAddr(v.Lit)
		if aerr != nil {
			return "", c.bad(field, "%q is not an address or a network", v.Lit)
		}
		p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
	}
	return "EXISTS (SELECT 1 FROM unnest(" + col + ") i WHERE i <<= " + c.arg(p.Masked().String()) + "::inet)", nil
}

// age reads 30m, 24h, 7d, 4w.
func age(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, false
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[len(s)-1]]
	return time.Duration(n) * unit, unit != 0
}

func (c *compiler) date(col, field string, v Value) (string, error) {
	if v.Regex || v.To != "" {
		return "", c.bad(field, "takes a date or an age, with an optional comparison")
	}
	if d, ok := age(v.Lit); ok {
		// An age is how long ago: <7d is newer than seven days ago.
		at := c.arg(c.now.Add(-d))
		switch v.Op {
		case "", "<":
			return col + " > " + at, nil
		case "<=":
			return col + " >= " + at, nil
		case ">":
			return col + " < " + at, nil
		case ">=":
			return col + " <= " + at, nil
		}
	}
	t, err := time.Parse(time.DateOnly, v.Lit)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, v.Lit); err != nil {
			return "", c.bad(field, "%q is not a date (2026-10-01) or an age (7d)", v.Lit)
		}
	}
	if v.Op == "" {
		return "(" + col + " >= " + c.arg(t) + " AND " + col + " < " + c.arg(t.Add(24*time.Hour)) + ")", nil
	}
	return col + " " + v.Op + " " + c.arg(t), nil
}

// tech binds a version to its own technology: the name and the version are
// tested on the same asset_tech row.
func (c *compiler) tech(t Term) (string, error) {
	name, err := c.anyOf(Term{Field: t.Field, Values: t.Values}, func(v Value) (string, error) {
		return c.text("t.name", t.Field, v)
	})
	if err != nil {
		return "", err
	}
	cond := name
	switch {
	case t.NoVersion:
		cond += " AND t.version IS NULL"
	case len(t.Versions) > 0:
		parts := make([]string, 0, len(t.Versions))
		for _, v := range t.Versions {
			p, err := c.version(v)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		cond += " AND (" + strings.Join(parts, " OR ") + ")"
	}
	return "EXISTS (SELECT 1 FROM asset_tech t WHERE t.asset_id = ac.asset_id AND " + cond + ")", nil
}

// bounds is the interval a partial version stands for: 1.24 is [1.24, 1.25).
func bounds(s string) (lo, hi []int32, ok bool) {
	lo = fingerprint.ParseVersion(s)
	if lo == nil {
		return nil, nil, false
	}
	hi = slices.Clone(lo)
	hi[len(hi)-1]++
	return lo, hi, true
}

func (c *compiler) version(v Value) (string, error) {
	if v.Regex {
		return "", c.bad("tech", "a version takes no regex")
	}
	lo, hi, ok := bounds(v.Lit)
	if !ok {
		return "", c.bad("tech", "%q is not a version", v.Lit)
	}
	col := "t.version"
	if v.To != "" {
		_, hiTo, ok := bounds(v.To)
		if !ok {
			return "", c.bad("tech", "%q is not a version", v.To)
		}
		return "(" + col + " >= " + c.arg(lo) + "::int[] AND " + col + " < " + c.arg(hiTo) + "::int[])", nil
	}
	switch v.Op {
	case "<":
		return col + " < " + c.arg(lo) + "::int[]", nil
	case "<=":
		return col + " < " + c.arg(hi) + "::int[]", nil
	case ">":
		return col + " >= " + c.arg(hi) + "::int[]", nil
	case ">=":
		return col + " >= " + c.arg(lo) + "::int[]", nil
	}
	return "(" + col + " >= " + c.arg(lo) + "::int[] AND " + col + " < " + c.arg(hi) + "::int[])", nil
}
