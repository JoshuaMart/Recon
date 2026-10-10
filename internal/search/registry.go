package search

// kind decides how a value is bound and which SQL a leaf compiles to.
type kind int

const (
	kindText kind = iota
	kindInt
	kindBool
	kindInet
	// kindUUID is bound as text and cast at the placeholder: a Go string against a
	// uuid column is refused by PostgreSQL, and casting in SQL keeps the value a
	// parameter instead of something this layer must parse.
	kindUUID
	kindTextArray
	kindTimestamp
	// kindJSONText is a scalar key of the attributes object, compiled to
	// containment only.
	kindJSONText
	// kindJSONArray is an array key of the same object, with the same containment
	// operator.
	kindJSONArray
	// kindJSONExists asks whether a key is there at all.
	kindJSONExists
)

// Alias is what the table is called in every query the compiler lands in.
// Expressions carry it directly: a rewriting pass would have to tell an
// identifier from the inside of a function call, and would silently get
// "reverse(key)" wrong, still valid SQL.
const Alias = "c"

// field is one entry of the vocabulary. The expression reaches SQL; a field name
// from a query never does. That is what "compiles to parameterized SQL" means
// here, and it makes adding a field trivial and deliberate.
type field struct {
	// expr is the SQL the field reads as, already qualified.
	expr string
	// key is the name inside the attributes object, for the JSON kinds. They compile
	// to containment over one column, so expression and key differ; conflating them
	// puts a key in a FROM clause.
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

// registry is the compiler's whole vocabulary. A field absent here is refused,
// so this table is also the answer to "what can be filtered". It is short on
// purpose.
//
// org_id is absent on purpose: the AST can neither filter on the tenant nor omit
// it, since the compiler emits that clause itself on every compilation.
var registry = map[string]field{
	// The suffix is the query an ASM inventory actually asks, served by the
	// expression index on reverse(key).
	//
	// Contains is for the console's search field, where "admin" means "anywhere in
	// the name". It is the one text operator no index answers, offered on these two
	// fields only, and scans one organization's inventory, not the table.
	"key":  {expr: Alias + ".key", kind: kindText, ops: ops(OpEq, OpPrefix, OpSuffix, OpContains)},
	"host": {expr: Alias + ".host", kind: kindText, ops: ops(OpEq, OpPrefix, OpSuffix, OpContains)},

	// The one identifier in the vocabulary, and not the tenant under another name.
	// A programme is a perimeter inside one organization: the switcher filters on
	// it and the policy still decides which programmes exist. It reads the leading
	// columns of the projection's existing index.
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

	// Three states; the third is an answer, not a gap. Null means no pass could look:
	// ingestion writes the flag only from a resolution that produced something,
	// since writing false from a timed-out name would clear it on a genuinely
	// fronted asset, and the upsert coalesces so null survives until something looks.
	//
	// So exists is on both and is the only way to ask: `not(eq is_cdn true)` cannot
	// reach it, as NOT (NULL = true) is NULL and excludes the row.
	"is_cdn":       {expr: Alias + ".is_cdn", kind: kindBool, ops: ops(OpEq, OpExists)},
	"waf_detected": {expr: Alias + ".waf_detected", kind: kindBool, ops: ops(OpEq, OpExists)},

	"ip": {expr: Alias + ".ip", kind: kindInet, ops: ops(OpEq, OpInCIDR)},

	"technologies": {expr: Alias + ".technologies", kind: kindTextArray, ops: ops(OpContains, OpIn)},

	"first_seen":      {expr: Alias + ".first_seen", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},
	"last_seen":       {expr: Alias + ".last_seen", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},
	"last_changed_at": {expr: Alias + ".last_changed_at", kind: kindTimestamp, ops: ops(OpBefore, OpAfter)},

	// The one field with no index. It reads a STABLE function of the bucket array
	// and the day it was last shifted, so a row that has not moved changes with the
	// calendar. It is evaluated per row, so it belongs in a query already narrowed
	// on something else.
	"volatility": {
		expr: "volatility(" + Alias + ".change_buckets, " + Alias + ".buckets_day)",
		kind: kindInt,
		ops:  ops(OpEq, OpGt, OpGte, OpLt, OpLte),
	},

	// Containment only, the one form the GIN index on attributes serves. "->>" would
	// put an unindexed scan behind an operator that looks indexed.
	"favicon_hash":   {key: "favicon_hash", kind: kindJSONText, ops: ops(OpEq)},
	"cert_spki_hash": {key: "cert_spki_hash", kind: kindJSONText, ops: ops(OpEq)},

	"script_hash":   {key: "script_hashes", kind: kindJSONArray, ops: ops(OpContains)},
	"cookie_name":   {key: "cookie_names", kind: kindJSONArray, ops: ops(OpContains)},
	"external_host": {key: "external_hosts", kind: kindJSONArray, ops: ops(OpContains)},
	// Written by the sweep, not a producer. Filterable because "which of my pages
	// load from a domain anybody can now register" is its whole purpose, and a
	// finding that cannot be listed is read once in an alert.
	"dead_external_host": {key: "dead_external_hosts", kind: kindJSONArray, ops: ops(OpContains)},

	"takeover_candidate": {key: "takeover_candidate", kind: kindJSONExists, ops: ops(OpExists)},

	// title is absent on purpose. It is promoted so the list can render a row, has
	// no index (like final_url), and the only wanted operator is a substring match,
	// i.e. a scan of the tenant. When it is asked for: an ALTER and a line here, in
	// that order.
}

// Fields is what an endpoint answers when asked what it accepts. A console that
// learns the vocabulary from 400s learns it wrong.
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
