// Package search compiles a structured query into parameterized SQL. An AST rather than a
// parser: the structured form comes first and a textual language can later produce the same
// tree, without freezing a syntax before anyone knows what gets filtered. Two rules hold
// throughout: a field name never reaches SQL as text (it is a key in a registry carrying the
// expression, type and permitted operators, so an unknown field is a refusal), and values are
// parameters, without exception.
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

// Node is one clause or one group. One type, since the tree is decoded from
// console JSON and a decoder guessing between two shapes guesses wrong on
// malformed input.
type Node struct {
	Op      string `json:"op"`
	Field   string `json:"field,omitempty"`
	Value   any    `json:"value,omitempty"`
	Clauses []Node `json:"clauses,omitempty"`
}

// Error is a refusal a caller can act on. One type for an unknown field,
// operator or wrongly shaped value, since the answer is the same: the registry
// does not describe this query.
type Error struct{ reason string }

func (e *Error) Error() string { return e.reason }

func refuse(format string, args ...any) error {
	return &Error{reason: fmt.Sprintf(format, args...)}
}

// Parse reads a tree and refuses anything the registry does not describe. It
// refuses at decode time so a console sending an unknown field gets one answer
// naming it, not an empty result it reads as an empty inventory.
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

// maxDepth bounds the tree. Not stylistic: the compiler recurses, so ten
// thousand nested "not" clauses would exhaust the stack instead of being
// refused. Real queries stay in single digits.
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
		// An empty group is only meaningful at the root, the request sent before any
		// facet is clicked. Nested, an empty AND is TRUE and an empty OR is FALSE, so
		// the same shape means everything in one parent and nothing in the other.
		if depth > 0 && len(n.Clauses) == 0 {
			return refuse("a %q group with nothing in it", n.Op)
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
		// The one place a field name is checked, against the registry and not a
		// pattern. org_id is not there at all: a tenant filter the caller can express is
		// one the caller can forget.
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
