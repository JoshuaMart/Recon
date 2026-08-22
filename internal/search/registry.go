package search

// kind decides how a value is bound and which SQL a leaf compiles to.
type kind int

const (
	kindText kind = iota
	kindInt
	kindBool
	kindInet
	// kindUUID is bound as text and cast at the placeholder. Bound as a Go
	// string against a uuid column, pgx sends it as text and PostgreSQL refuses
	// the comparison; casting in the SQL is what keeps the value a parameter
	// rather than something this layer has to parse to prove it is one.
	kindUUID
	kindTextArray
	kindTimestamp
	// kindJSONText is a scalar key of the attributes object, and it compiles to
	// containment and to nothing else.
	kindJSONText
	// kindJSONArray is an array key of the same object, and it compiles to the
	// same containment operator.
	kindJSONArray
	// kindJSONExists asks whether a key is there at all.
	kindJSONExists
)

// Alias is what the table is called in every query the compiler lands in.
//
// The expressions below carry it rather than a rewriting pass adding it later.
// A pass like that has to tell an identifier from the inside of a function call,
// and it gets "reverse(key)" wrong on the first try, silently, because the
// result is still valid SQL.
const Alias = "c"

// field is one entry of the vocabulary.
//
// The expression is what reaches SQL, and a field name from a query never does.
// That is what "compiles to parameterized SQL" means here, and it is also what
// makes adding a field both trivial and deliberate.
type field struct {
	// expr is the SQL the field reads as, already qualified.
	expr string
	// key is the name inside the attributes object, for the JSON kinds. Those
	// compile to containment over one column, so the expression and the key are
	// two different things and conflating them is how a key ends up in a FROM
	// clause.
	key  string
	kind kind
	ops  map[string]bool
}

func ops(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// registry is the whole of the compiler's vocabulary.
//
// A field absent from here is a refusal, so this table is also the answer to
// "what can be filtered". It is short on purpose.
//
// org_id is not in it, and that is the point rather than an omission. The AST
// cannot express the tenant, neither to filter on it nor to omit it: the
// compiler emits that clause itself, outside the tree, on every compilation.
var registry = map[string]field{
	// The suffix is the query an ASM inventory actually asks, and it is served
	// by the expression index on reverse(key).
	"key":  {expr: Alias + ".key", kind: kindText, ops: ops(OpEq, OpPrefix, OpSuffix)},
	"host": {expr: Alias + ".host", kind: kindText, ops: ops(OpEq, OpPrefix, OpSuffix)},

	// The one identifier in the vocabulary, and it is not the tenant wearing
	// another name. org_id is absent because the compiler emits it and a query
	// must not be able to name it in either direction; a programme is a
	// perimeter inside one organization, the switcher on every screen is a
	// filter on it, and the policy behind it still decides which programmes
	// exist. It reads the leading columns of the index the projection already
	// carries.
	"program_id": {expr: Alias + ".program_id", kind: kindUUID, ops: ops(OpEq, OpIn)},

	"kind":         {expr: Alias + ".kind", kind: kindText, ops: ops(OpEq, OpIn)},
	"lifecycle":    {expr: Alias + ".lifecycle", kind: kindText, ops: ops(OpEq, OpIn)},
	"scope_status": {expr: Alias + ".scope_status", kind: kindText, ops: ops(OpEq, OpIn)},
	"scheme":       {expr: Alias + ".scheme", kind: kindText, ops: ops(OpEq, OpIn)},

	"port":        {expr: Alias + ".port", kind: kindInt, ops: ops(OpEq, OpIn, OpGt, OpGte, OpLt, OpLte)},
	"status_code": {expr: Alias + ".status_code", kind: kindInt, ops: ops(OpEq, OpIn, OpGt, OpGte, OpLt, OpLte)},
	"asn":         {expr: Alias + ".asn", kind: kindInt, ops: ops(OpEq, OpIn, OpGt, OpGte, OpLt, OpLte)},

	"country":      {expr: Alias + ".country", kind: kindText, ops: ops(OpEq, OpIn)},
	"cdn_provider": {expr: Alias + ".cdn_provider", kind: kindText, ops: ops(OpEq, OpIn)},
	"waf_vendor":   {expr: Alias + ".waf_vendor", kind: kindText, ops: ops(OpEq, OpIn)},
	"server":       {expr: Alias + ".server", kind: kindText, ops: ops(OpEq, OpIn)},

	"is_cdn":       {expr: Alias + ".is_cdn", kind: kindBool, ops: ops(OpEq)},
	"waf_detected": {expr: Alias + ".waf_detected", kind: kindBool, ops: ops(OpEq)},

	"ip": {expr: Alias + ".ip", kind: kindInet, ops: ops(OpEq, OpInCIDR)},

	"technologies": {expr: Alias + ".technologies", kind: kindTextArray, ops: ops(OpContains, OpIn)},

	"first_seen":      {expr: Alias + ".first_seen", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},
	"last_seen":       {expr: Alias + ".last_seen", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},
	"last_changed_at": {expr: Alias + ".last_changed_at", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},

	// The one field with no index, and it is in anyway. It reads a STABLE
	// function of the bucket array and the day it was last shifted, so it
	// cannot be indexed at all: the value of a row that has not moved changes
	// with the calendar. It is evaluated per row, which is why it belongs in a
	// query that already narrows on something else.
	"volatility": {
		expr: "volatility(" + Alias + ".change_buckets, " + Alias + ".buckets_day)",
		kind: kindInt,
		ops:  ops(OpEq, OpGt, OpGte, OpLt, OpLte),
	},

	// Containment, and nothing else. That is the one form the GIN index on
	// attributes serves, and offering "->>" beside it would put an unindexed
	// full scan behind an operator indistinguishable from the indexed one.
	"favicon_hash":   {key: "favicon_hash", kind: kindJSONText, ops: ops(OpEq)},
	"cert_spki_hash": {key: "cert_spki_hash", kind: kindJSONText, ops: ops(OpEq)},

	"script_hash":   {key: "script_hashes", kind: kindJSONArray, ops: ops(OpContains)},
	"cookie_name":   {key: "cookie_names", kind: kindJSONArray, ops: ops(OpContains)},
	"external_host": {key: "external_hosts", kind: kindJSONArray, ops: ops(OpContains)},
	// Written by the sweep rather than by a producer, and filterable for the
	// same reason the finding is critical: "which of my pages load from a
	// domain anybody can now register" is the question it exists to answer, and
	// a finding that cannot be listed is one somebody reads once in an alert.
	"dead_external_host": {key: "dead_external_hosts", kind: kindJSONArray, ops: ops(OpContains)},

	"takeover_candidate": {key: "takeover_candidate", kind: kindJSONExists, ops: ops(OpExists)},

	// title is deliberately absent, and its absence is the rule working rather
	// than an oversight. It is a promoted column so the list can render a row,
	// it carries no index by the same decision that left final_url without one,
	// and the only operator anybody would want on it is a substring match,
	// which is a scan of the tenant. The day the query is asked for it is an
	// ALTER and a line here, in that order.
}

// Fields is what an endpoint answers when asked what it accepts.
//
// A console that has to learn the vocabulary against 400s is a console that
// learns it wrong, and this is the same reason the enrichment state is served
// rather than deduced.
func Fields() map[string][]string {
	out := make(map[string][]string, len(registry))
	for name, entry := range registry {
		accepted := make([]string, 0, len(entry.ops))
		for op := range entry.ops {
			accepted = append(accepted, op)
		}
		out[name] = accepted
	}
	return out
}
