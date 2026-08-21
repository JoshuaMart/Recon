// Package search compiles a structured query into parameterized SQL.
//
// An AST rather than a parser. The structured representation is defined first
// and a textual query language comes later and produces the same tree, which
// avoids freezing a syntax before anybody knows what actually gets filtered.
//
// Two rules hold everywhere in here. A field name never reaches SQL as text: it
// is a key in a registry carrying the expression, the type and the permitted
// operators, so an unknown field is a refusal rather than a query. And values
// are parameters, without exception.
package search

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Boolean operators, which carry clauses rather than a field.
const (
	OpAnd = "and"
	OpOr  = "or"
	OpNot = "not"
)

// Leaf operators. What each one means is decided by the field's type, and the
// registry is what says which of them a field accepts.
const (
	OpEq       = "eq"
	OpIn       = "in"
	OpGt       = "gt"
	OpGte      = "gte"
	OpLt       = "lt"
	OpLte      = "lte"
	OpPrefix   = "prefix"
	OpSuffix   = "suffix"
	OpContains = "contains"
	OpExists   = "exists"
	OpBefore   = "before"
	OpAfter    = "after"
	OpInCIDR   = "in_cidr"
)

// Node is one clause or one group of them.
//
// One type rather than two, because the tree is decoded from JSON a console
// sends and a decoder that has to guess which of two shapes it is holding is a
// decoder that guesses wrong on the malformed case.
type Node struct {
	Op      string `json:"op"`
	Field   string `json:"field,omitempty"`
	Value   any    `json:"value,omitempty"`
	Clauses []Node `json:"clauses,omitempty"`
}

// Error is a refusal a caller can act on.
//
// It is the same type for an unknown field, an unknown operator and a value of
// the wrong shape, because the answer to the outside is the same: this query is
// not one the registry describes.
type Error struct{ reason string }

func (e *Error) Error() string { return e.reason }

func refuse(format string, args ...any) error {
	return &Error{reason: fmt.Sprintf(format, args...)}
}

// Parse reads a tree and refuses anything the registry does not describe.
//
// Refusing at decode time rather than at compile time is deliberate: a console
// that sends a field nobody knows gets one answer naming it, rather than an
// empty result set it will read as an empty inventory.
func Parse(raw []byte) (Node, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Node{Op: OpAnd}, nil
	}

	var node Node
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&node); err != nil {
		return Node{}, refuse("the query is not a filter tree: %v", err)
	}
	if err := node.validate(0); err != nil {
		return Node{}, err
	}
	return node, nil
}

// maxDepth bounds the tree.
//
// Not a stylistic limit. The compiler walks it recursively, so an attacker
// sending ten thousand nested "not" clauses would exhaust the stack of the
// process rather than be refused, and the depth a real query reaches is single
// digits.
const maxDepth = 16

// maxClauses bounds one group.
const maxClauses = 64

func (n Node) validate(depth int) error {
	if depth > maxDepth {
		return refuse("the query nests deeper than %d levels", maxDepth)
	}

	switch n.Op {
	case OpAnd, OpOr, OpNot:
		if n.Field != "" {
			return refuse("%q is a group and carries the field %q", n.Op, n.Field)
		}
		if len(n.Clauses) > maxClauses {
			return refuse("a group carries %d clauses, and the bound is %d", len(n.Clauses), maxClauses)
		}
		if n.Op == OpNot && len(n.Clauses) == 0 {
			return refuse("a negation with nothing to negate is not a query")
		}
		for _, clause := range n.Clauses {
			if err := clause.validate(depth + 1); err != nil {
				return err
			}
		}
		return nil

	case "":
		return refuse("a clause with no operator")

	default:
		if len(n.Clauses) > 0 {
			return refuse("%q is a leaf and carries clauses", n.Op)
		}
		// The one place a field name is checked, and it is checked against the
		// registry rather than against a pattern. An organization filter the
		// caller can express is one the caller can forget, which is why org_id
		// is not in there at all.
		entry, known := registry[n.Field]
		if !known {
			return refuse("no field named %q", n.Field)
		}
		if !entry.ops[n.Op] {
			return refuse("%q does not accept %q", n.Field, n.Op)
		}
		return nil
	}
}
