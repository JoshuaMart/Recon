---
title: Search and filters
description: Query the current state rather than the journal, compile an AST rather than parse a language, and pay for facets knowingly.
sidebar:
  order: 10
---

## 10.1 Three principles

**1. Never query the observation journal.** The interface reads `asset_current`. Observations exist for
history and diff, and [10.9](#109-the-asset-view) is the single, deliberate exception.

**2. Promoted columns plus JSONB.** The hot fields, `host`, `port`, `scheme`, `status_code`,
`cdn_provider`, `asn`, `country`, the layer states, live in typed indexed columns. The rest lives in
JSONB behind a GIN index. Flexibility without giving up indexes on what is filtered 90 % of the time.

**3. An AST, not a parser.** Define the structured representation first, and compile it to parameterized
SQL:

```json
{
  "op": "and",
  "clauses": [
    {"field": "key",         "op": "suffix",   "value": ".target.com"},
    {"field": "port",        "op": "eq",       "value": 443},
    {"field": "technologies","op": "contains", "value": "nginx"},
    {"op": "not", "clauses": [{"field": "is_cdn", "op": "eq", "value": true}]}
  ]
}
```

A textual query language comes later and produces the same tree. That avoids freezing a syntax before
knowing what actually gets filtered.

## 10.2 What the projection carries

A facet aggregates over what the table holds, and a counter maintained on write cannot be maintained if
the write never sees the value. So the projection has to lift the pivots out of the observation payload,
and that is the precondition for everything else in this chapter.

**Into `attributes`, except `technologies`.** These values are filtered by exact equality on short text,
and the existing GIN index covers `attributes @> '{"favicon_hash": "..."}'` without adding five columns,
three of which would be arrays. Principle 2 reserves a promoted column for what most queries filter on,
and a `favicon_hash` is an occasional pivot rather than a common filter.

`technologies` is the exception, for the opposite reason: it is filtered constantly, and it already has a
column and an index.

```json
{
  "favicon_hash": "8f3a1c...",
  "cert_spki_hash": "2953e9...",
  "script_hashes": ["a4f3e2...", "b71c04..."],
  "cookie_names": ["SESS_INTERNAL", "csrftoken"],
  "external_hosts": ["cdn.example.net"]
}
```

**The column holds the names, `attributes` holds the versions.** `technologies` is a GIN indexed `text[]`,
queried by exact element, so `["nginx 1.24"]` would not answer a filter on `nginx`, which the AST above
asks for explicitly. The versions the interface shows travel alongside as `{name, version}` objects. The
column is the index and the facet; the object is the evidence.

### Technologies have two producers, and that is the one place the rule bends

Every other value in this chapter has one. This one cannot, and the reason is that the two arguments used
elsewhere point in opposite directions here.

**Coverage says the probe has to contribute.** It is the same argument that puts `cert_spki_hash` on the
`http` layer: the probe sees every service on every full pass, while a render happens on
[five triggers](/architecture/verification/#83-when-a-render-happens) that can be three weeks apart or never.
An inventory whose technology filter only knew what a browser saw would be blind on most of itself, and
blind precisely on the assets no browser can open, which is where the interesting targets sit.

**Depth says the render has to contribute.** A rendered page shows what a raw fetch cannot, and dropping
that would make the richest detection in the system unfilterable.

So both write, and the rule is kept where it actually matters: **they write different keys.** The probe's
names land in `attributes.tech_http`, the render's `{name, version}` objects in `attributes.tech_render`,
and neither can erase the other. What would break the rule is two producers writing the same value, which
is what a single shared key would be, and which would make every pass of one layer overwrite the other's
work and churn deduplication on the busiest table in the system.

**The column is the union of the names**, recomputed from those two keys in the same statement that writes
them. That is what a filter wants: "which of my assets run nginx" answers yes whether the probe or the
browser saw it, and neither producer's absence turns into a false no.

The version is only ever the render's, because only the render reports one. A technology known by name and
not by version is the normal case and reads as such rather than as missing data.

### The redirect chain and the final URL are promoted columns

Showing `200` on a service is true without being the information. The probe may have obtained a 308, then
a 307, then a 200, and landed on a different path or a different host. `http://host` ending at
`https://host/login` is an application behind a redirect; `http://host` ending on another host is an
entirely different thing, and the only place the difference shows is the last hop.

So `status_chain` and `final_url` are columns rather than `attributes`, and the justification is not
filtering. It is the precedent of `title` and `status_code`: typed columns, unindexed, written by the
`http` layer, existing so the list can render a row without reading `observation`. Filing them in the JSON
blob would make them more expensive to read for the sole reason that nobody filters on them.

**The two have different producers, because they have different observers.** `final_url` is the probe's: it
reports where it landed, and it sees every service on every pass. `status_chain` is the render's, and not by
preference. The scanner's contract reports the redirect **URLs** and the final code, never the code of each
hop, so the probe does not hold the information the column is for; the browser reports one hop per entry with
its status, so it does. Writing the column from the probe would mean inventing the intermediate codes, which
is the one thing worse than an empty column.

The cost is the usual one and is stated rather than discovered: the chain is present only on assets a render
has reached, so it is missing on the [protected regime](/architecture/verification/#86-reachability-per-observer)
exactly where the rest of the fingerprinter's output is missing. The row's `status_code` keeps full coverage,
which is why it is the one the list leads with, and the chain is the detail beside it. Like every other value
the render produces, it carries the fingerprinter's timestamp rather than the row's.

**No index on `final_url`.** A "ends on /login" filter is conceivable and nobody has asked for it, and the
index would cost on every write of the verification loop. The day the query exists, it is an `ALTER`.

**A single hop chain is not displayed as a chain.** `{200}` is a 200, and an arrow pointing at nothing is
noise on the majority of rows. Likewise a `final_url` equal to the base URL is not written twice: what is
shown is the difference, never the repetition.

### The scheme comes from the observation, not from the port

A TLS listener on 8080 and a cleartext listener on 8443 both exist, so the port number says nothing. But
the *system* knows: the probe requested a specific URL and got an answer, so the scheme is **measured** and
only needs keeping. FastRecon reports it, and omits the port from the URL when it is the scheme's default,
which is what makes an unusual finding stand out instead of drowning in redundant `:443` suffixes.

`scheme` is therefore a promoted column, written by the `http` layer from the URL the probe actually
requested, the first hop and not the final URL: a redirect to `https` describes where the service sends you,
not how it is addressed. Filterable and facetable in the same move.

**A service with no scheme is a service no HTTP probe made answer**, and it keeps its `host:port` form. The
absence is then exact rather than cautious.

Deriving a display category the search cannot express is worse than not having the category: a visible
distinction that is not queryable is worse than an absent one.

### The favicon image is not in `attributes`

A favicon is by far the fastest identity signal to read in an inventory, which is why Fofa and Shodan put
it at the head of every row. Three ways to get it to the list, and the first two fail on numbers:

- **In `attributes`.** One or two kilobytes per asset, in the hottest write path of the system and in every
  search response. Gigabytes on an inventory of a million. And that column is reserved for what is filtered:
  an image is neither a filter nor a pivot, it is the depiction of a pivot that already exists.
- **Read from the journal while rendering the list.** One round trip per row, fifty per page, for a
  thumbnail.
- **A side table keyed by `(org_id, favicon_hash)`.** One copy per distinct favicon, however many assets
  share it, which is exactly the property that matters since a shared favicon is the interesting case. The
  list joins it once per page.

It is the third, and its shape is copied from `pivot_count` on purpose: same key, same per organization
scope, written in the same transaction as the projection. Writing costs nothing in steady state, since the
statement is only emitted when there is something new and `ON CONFLICT DO NOTHING` makes a known favicon a
no-op.

**A size bound, because the value is chosen by the target.** Nothing stops a server from serving five
megabytes under the name of a favicon. Past a limit the image is not stored: the hash and its counter keep
working, only the thumbnail is missing. That is honest degradation, and it is worth writing here rather than
discovering on a storage bill.

This is not a screenshot cache. A screenshot must never reach PostgreSQL and normalization drops it. The
difference is not one of degree: a favicon is a kilobyte or two, it is shared between assets, and it is
already a pivot of the model.

### One producer per value, here too

| Projected key | Layer | Path in the payload |
|---|---|---|
| `favicon_hash` | `fingerprint` | `metadata.favicon_hash` |
| `script_hashes` | `fingerprint` | `scripts[].hash`, **`internal` only** |
| `cookie_names` | `fingerprint` | keys of `cookies` |
| `technologies` | `fingerprint` | `technologies[].name` |
| `external_hosts` | `fingerprint` | `external_hosts` |
| `cert_spki_hash` | `http` | `tls.cert_spki_hash` |

The `internal` flag on scripts is not a reading detail. A bundle served from a public CDN is shared by
thousands of unrelated sites: it groups without discriminating, which is the test that decides what is a
pivot at all.

The case not to miss is cookies. The `http` layer could report cookie names too, and it is **not** projected:
the pivot belongs to the fingerprinter, which also sees the cookies a script sets. Projecting both
would count the same asset twice under the same name.

## 10.3 The compiler, and what it does not delegate

**A registry, not a translation.** A field name from a query never reaches SQL as text: it is a key in a
table carrying the expression, the type and the permitted operators. An unknown field is a refusal, not a
query. Values are parameters, without exception. That is what "compiles to parameterized SQL" means, and it
is also what makes adding a field both trivial and deliberate.

**An empty group is only meaningful at the root.** There it is the request an interface sends before
anybody has clicked a facet, and the whole tenant is the right answer. Nested it is a refusal, because an
empty `and` is true by identity and an empty `or` is false, so the same shape means the whole inventory
under one parent and nothing at all under the other. The case that settles it is the negation: `not` of an
empty group is "not everything", which is no rows, and a compiler letting an empty group contribute
nothing answers the entire inventory instead. Refusing names the group; picking one silently answers a
different question.

**The tenant is not a clause.** `org_id` does not exist in the registry, so the AST cannot express it, neither
to filter on it nor to omit it. The compiler emits it itself, outside the tree, on every compilation. An
organization filter the caller can express is one the caller can forget.

This query does not live in the static query files, so the static tenant guard of
[11.1](/architecture/security/#111-irreversible-decisions) does not see it. Saying so is better than implying
an inventory that is no longer exhaustive. What holds instead, in order of strength: the organization clause
is structural and a test requires it on *every* compilation, including that of an empty AST; then
[Row-Level Security](/architecture/security/#row-level-security-two-roles-rather-than-one-variable), which is
the only guarantee the compiler cannot remove from itself.

### What the registry holds

The registry is the whole of the compiler's vocabulary. A field absent from it is a refusal, so this table is
also the answer to "what can be filtered", and it is short on purpose.

| Field | Reads | Type | Operators |
|---|---|---|---|
| `key`, `host` | the column | text | `eq`, `prefix`, `suffix`, `contains` |
| `program_id` | the column | uuid | `eq`, `in` |
| `kind`, `lifecycle`, `scope_status`, `scheme` | the column | text | `eq`, `in` |
| `port`, `status_code`, `asn` | the column | int | `eq`, `in`, `gt`, `gte`, `lt`, `lte` |
| `country`, `cdn_provider`, `waf_vendor`, `server` | the column | text | `eq`, `in` |
| `is_cdn`, `waf_detected` | the column | bool | `eq`, `exists` |
| `ip` | the column | inet | `eq`, `in_cidr` |
| `technologies` | the column | text[] | `contains`, `in` |
| `first_seen`, `last_seen`, `last_changed_at` | the column | timestamptz | `before`, `after` |
| `volatility` | the bucket function | int | `eq`, `gt`, `gte`, `lt`, `lte` |
| `favicon_hash`, `cert_spki_hash` | `attributes` | text | `eq` |
| `script_hash`, `cookie_name`, `external_host` | `attributes` | text[] | `contains` |
| `dead_external_host` | `attributes` | text[] | `contains` |
| `takeover_candidate` | `attributes` | bool | `exists` |

**`is_cdn` and `waf_detected` take `exists`, because a nullable boolean has three states and the third is an
answer.** Null means no pass has been able to look: ingestion writes the flag only from a resolution that
produced an address, a CNAME or a provider, since writing `false` from a name that timed out would clear it
on an asset that is genuinely behind an edge, and the upsert coalesces so the null survives until something
looks. A candidate from Certificate Transparency and a hand entered host both sit there until then.

`exists` is the only way to ask. The obvious spelling, the negation used as the example in
[10.1](#101-three-principles), does not reach it and cannot: `NOT (NULL = true)` is `NULL` in SQL and a null
predicate excludes the row. So "everything that is not fronted" written as a negation looks complete and
quietly drops every asset nobody has looked at. What that sentence usually means is the union:

```json
{"op": "or", "clauses": [
  {"field": "is_cdn", "op": "eq",     "value": false},
  {"field": "is_cdn", "op": "exists", "value": false}
]}
```

The operator reads the same as it does over `attributes`, "is there a value for this", against a column
instead of a key. Its `value` is required on both and decides the direction rather than being decoration.

**`program_id` is in the registry and `org_id` is not**, and the pair is worth a paragraph because they look
like the same kind of field. The organization is emitted by the compiler on every compilation, so a query can
neither name it nor omit it: a tenant filter a caller can express is one a caller can forget. A program is a
perimeter **inside** one organization, the switcher sits on every screen and is exactly a filter on it, and
the tenant clause is still emitted beside it, so naming somebody else's program returns nothing rather than
their inventory. It reads the leading columns of an index the projection already carries.

The value is bound and cast at the placeholder rather than parsed first. The cast is what refuses a malformed
identifier, with the value still a parameter, and parsing it here would be a second definition of what a
uuid is.

**The JSONB fields compile to containment and nothing else**, `attributes @> '{"favicon_hash": "..."}'`, on
a scalar key and on an array key alike. That is the one form the GIN index of
[10.2](#102-what-the-projection-carries) serves, and offering `->>` beside it would put an unindexed
full scan behind an operator indistinguishable from the indexed one.

**`dead_external_host` is written by the sweep rather than by a producer**, and it is in the registry for
the same reason the finding is critical: "which of my pages load from a domain anybody can now register" is
the question it exists to answer, and a finding that cannot be listed is one somebody reads once, in an
alert, at nine in the evening.

**`title` is not in the registry**, and its absence is the rule working rather than an oversight. It is a
promoted column so the list can render a row, it carries no index by the same decision that left `final_url`
without one, and the only operator anybody would want on it is `contains`, which is a scan of the tenant.
The name gets that operator and the title does not ([10.3](#contains-is-granted-on-the-name-and-it-is-the-one-operator-no-index-answers)):
one is what people search by and the other is what a page happens to call itself.
The day the query is asked for, it is an `ALTER` and a line here, in that order.

**`volatility` is the one field with no index and it is in anyway.** It reads a `STABLE` function of the
bucket array and the day it was last shifted, so it cannot be indexed at all: the value of a row that has
not moved changes with the calendar. It is evaluated per row, which is why it belongs in a query that
already narrows on something else, and saying so here is better than someone measuring it later.

### The suffix is the query that matters

A `text_pattern_ops` index accelerates a **prefix**. The query of an ASM inventory is a **suffix**,
`.target.com`, meaning everything under this domain. A `LIKE '%.target.com'` can use no index and costs a full
scan of the tenant.

An **expression index on `reverse(key)`** turns one into the other: the suffix becomes a prefix on the reversed
string. No column, no data rewrite, one index migration, and `reverse` is immutable so it is indexable.

**The index is on `host` as well as on `key`, and that is not symmetry.** A service is keyed
`app.target.com:443/tcp` ([4.3](/architecture/data-model/#the-unit-of-a-web-asset-is-the-service-never-the-path)),
so `.target.com` is a suffix of the *name* and not of the key: the query that reads "everything under this
domain" returns the fqdn rows and silently drops every service, which is most of an inventory and all of the
interesting part. `host` is the column that answers it, so `host` is the column that needs the reversed
index too. The example in [10.1](#101-three-principles) filters on `key`, and it is the narrower query:
`key` answers "this exact service", `host` answers "this perimeter".

### `contains` is granted on the name, and it is the one operator no index answers

The console has a search field, and what somebody types in it is a substring: "admin" means "anywhere in the
name", not "the name starts with it". That compiles to `ILIKE '%admin%'`, a leading wildcard no index can
serve, so it costs a scan of the rows the rest of the filter left.

It is granted on `key` and `host` and on nothing else, for two reasons. The name is what a person searches by,
so the field that gets the expensive operator is the one that earns it. And the set it scans is one
organization's inventory, already narrowed by the tenant clause and by whatever facets are in force, which on
the perimeters this is built for is thousands of rows rather than a table. The day that stops being true, the
honest fix is a trigram index on those two columns, not a quieter operator.

`ILIKE` rather than `LIKE`, because the column holds a normalized name and the person typing does not.

Deliberately kept: the suffix stays a **string** suffix, not domain membership. `.target.com` does not return
`target.com` itself, and `evil-target.com` does not come back under `target.com` since the dot is in the
pattern. A notion of domain gets added if it turns out to be missing; inventing it now would freeze scope
semantics in the place where one is merely searching.

:::caution[Escape after reversing, not before]
Escaping the LIKE wildcards before reversing puts each backslash after the character it should protect, so
`_` stays a live wildcard and a trailing backslash escapes the appended `%`. The search silently becomes an
equality. No test sees it unless a test value contains a wildcard.
:::

## 10.4 Facets are the real cost

The side counters of an ASM interface, "port 443: 363", "HSTS: 54", "AWS: 65", are not global statistics.
They are **aggregations over the current filtered result**, recomputed on every query. That is what usually
pushes projects toward a search engine.

In PostgreSQL, with the right indexes, this holds comfortably to a few million rows per tenant. Stay there at
the start. A double write to Elasticsearch on day one is a classic trap.

Optimizations to reach for before migrating: precomputed partial aggregates, `GROUP BY` on promoted columns
only, and a cap on how many facet values come back.

**The set is fixed and it is short.** `lifecycle`, `kind`, `port`, `scheme`, `status_code`, `country`,
`asn`, `cdn_provider`, `waf_vendor`, `technologies`, which is the only one over an array, and `favicon_hash`,
which is the only one over `attributes`.

**That last one was refused first, on a reason that did not survive the next paragraph.** The argument was
that a facet over `attributes` would have to aggregate through a GIN index that answers containment and
cannot group. It describes an implementation this never had: the filter runs once into a CTE and every facet
groups over that, so no facet is served by its own index and a key of the object costs exactly what a column
costs. The index serves the filter; a facet is an aggregation over what the filter already produced.

The favicon earns its place because it is the one pivot a reader wants as a **list** rather than one badge at
a time. "Which icons does this perimeter share, and how many assets each" is the fastest identity signal an
inventory has, and without the facet the only way to ask it was to click a badge on a row that happened to
carry one.

**The images travel with the facet**, not with the page's rows. The two sets are not the same: a facet ranks
the whole filtered result and a page shows fifty of it, so the most shared icon in a perimeter is routinely
one no row on screen carries. Taken from the rows, that entry draws as a blank square with a count beside it,
which reads as a broken image rather than as the answer.

**One statement, not ten.** The facets are computed in a single pass over the filtered set rather than one
query per facet, because the expensive half is the filter and running it ten times is paying for it ten
times. The filter is compiled once, into a CTE, and each facet groups over it.

**Twenty values per facet, and the cut says so.** A truncated facet that looks complete makes somebody
believe the inventory holds nine ports. The response carries the values, the count of each, and whether
anything was left out, which is the same rule the export applies to rows and the timeline applies to
observations.

**The cut has to be openable, or it is a wall.** Saying "20+" is honest and it is not enough: a technology
carried by twelve assets is in the inventory, is filterable, and had nothing to click, so the only way to
reach it was to know it existed and to write the filter by hand. A `field` on the same request answers that
one facet on its own, bounded at two hundred instead of twenty. Same aggregation over the same filtered set,
so the counts beside the values are the counts the sidebar would have shown, and the field is matched against
the facet table rather than reaching the statement as a string.

Two hundred and not everything, because the cut stays expressible either way: a list that ends without saying
it ended is the failure the cap exists to avoid. It costs no more of the expensive half than the sidebar
does, since the filter is what is expensive and both aggregate over the same CTE.


## 10.5 Pivots

A hash is not only a change detector. It is a **join key between assets**. Several of them group assets that
nothing else connects:

| Pivot | Source | Granularity |
|---|---|---|
| `favicon_hash` | fingerprinter | one value |
| `cert_spki_hash` | `http` layer | one value |
| `scripts[].hash` | fingerprinter | per internal script |
| cookie name | fingerprinter | per name, outside the denylist |

**A pivot must discriminate, not merely group.** That is what rules out a TLS fingerprint such as JARM: on a
perimeter mostly fronted by a CDN, every asset of one provider shares it, so the pivot returns hundreds of
generic results and is not actionable. Compare with an internal bundle hash, which groups the handful of hosts
running the same application.

**The certificate key is measured by the HTTP layer and not by the render**, and the reason is coverage
rather than tidiness. A browser render would get the hash for free, since it already completed the
handshake, while the probe pays one extra handshake per HTTPS service. But the probe sees **every** HTTPS
service on every full pass, and a render happens on five triggers that can be three weeks apart, or never on
an asset that has not earned a baseline. A pivot present on a fraction of the inventory joins nothing, and the
fraction it would miss is the [protected regime](/architecture/verification/#86-reachability-per-observer),
which is where the interesting targets tend to sit. The extra handshake is the price of the coverage that
makes it a pivot at all.

There is a second reason, smaller and still decisive: a browser follows redirects, so "which certificate"
stops having one answer. The probe records a certificate only for a connection that was itself TLS, on the
port being described, which is the right subject.

A hash is kept in the model only if it serves as a pivot. Anything that was merely change detection is compared
in the clear by the Notifier ([12.1](/architecture/notifications/#121-structured-diffs)).

Every displayed value carries a counter, "this favicon appears on 12 other assets", and clicking it runs the
matching search.

**The counter is the problem.** Computed on the fly it means one `COUNT(*)` per displayed value, which is
unworkable as soon as a page shows dozens. Hence an aggregate table maintained on write:

```sql
CREATE TABLE pivot_count (
  org_id      uuid NOT NULL,
  pivot_type  text NOT NULL,   -- favicon | script | cert_spki | cookie_name
  pivot_value text NOT NULL,
  count       int  NOT NULL,
  PRIMARY KEY (org_id, pivot_type, pivot_value)
);
```

Incremented when an asset acquires a value, decremented when it loses one, in the same transaction that
rewrites `asset_current`, which still holds the old value at the moment of the diff. The counter is **per
organization**, never global: a tenant must not infer the size of another's inventory from a number.

### Decrementing, and the path that gets forgotten

Incrementing is the easy half. A counter only drifts through the decrement, and it drifts **upward**: a pivot
announced at 41 that now links 12 is worse than an absent pivot, since it sends someone looking for thirty hosts
that do not exist. The badge keeps asserting, and it asserts falsely.

Two paths produce it, and the second is the one that gets forgotten:

- **Losing a value.** An asset that changes favicon decrements the old and increments the new. That is the diff
  already in hand at write time.
- **Archiving.** An asset going `archived` gives back **all** its pivots although no value changed. A pivot is a
  lead to follow, and an archived asset is not a lead. Nothing in the payload comparison signals this: it is the
  `lifecycle` transition that says so, and it travels in the same statement as the projection.

An asset is never deleted ([P3](/architecture/principles/)), so there is no third path. Manual reactivation is
the inverse and must re-increment, otherwise the drift merely changes direction.

**Today only one kind of asset can take that second path, and saying so is better than implying otherwise.**
Archiving is decided when a **candidate host** exhausts its budget without ever coming alive, and a host
that never came alive has no service, no render and therefore no pivot. So the decrement is a guarantee for
the paths that are coming, the hand archived asset and the
[Certificate Transparency candidate](/architecture/roadmap/) that never resolves, rather than something that
fires on an ordinary week. It is built now because retrofitting a counter's decrement means first working
out how far it has drifted.

**The invariant that makes all of this checkable**: `pivot_count` is a function of the counted keys of
`asset_current`, and of nothing else. A counter that reflects nothing cannot be repaired, for lack of anywhere to
read the truth; this one recomputes entirely from a scan of the table the day anyone doubts it.

Archiving therefore has to **remove those keys from the asset** as well as decrement. Without that, an archived
asset still receiving an observation, which nothing forbids, would have its pivots rewritten and recounted.
`technologies` and `external_hosts` stay in place: they carry no counter, they are a filter and an aggregation,
and the invariant speaks only about what is counted.

### The genericity filter

Filtering by local frequency does not work, even relative to inventory size, because on a small perimeter the
variance dominates. With 200 assets, `PHPSESSID` at 3 occurrences and a rare application cookie at 2 are
statistically indistinguishable.

The relevant quantity is frequency **on the internet**, not inside the organization. `PHPSESSID` is noise because
it is universal, not because it is locally frequent. Hence a **static denylist, versioned in the repository**:
frameworks, analytics, CDN and WAF cookies, CMS cookies. A hundred entries or so.

**A display denylist, never a collection one.** Every name is indexed without exception; the list only removes the
badge from the row. A misclassified entry loses no data, and the explicit search still works.

The mechanism is not specific to cookies, which is why the table carries a `pivot_type`: a default Apache or nginx
favicon, or a public CDN bundle, pose the same problem.

**A complementary guardrail**: no badge is shown when its counter is 1. A pivot leading only to itself has no value.

**Feeding it.** The table is the reflection of a repository file, never an autonomous source. A versioned YAML file
is the truth, applied by an **idempotent, replayable seed** run on every deployment rather than by a one off
migration, since the list grows continuously and must not produce a migration per addition. The semantics is
**full replacement**, not merge: a merge would let any entry added outside the repository survive indefinitely and
make the divergence invisible. Write privileges on the table are revoked for the application role.

**Maintenance.** A global view across all organizations identifies candidates: a value at the top of the ranking and
absent from the list is probably generic. Group on `(pivot_type, pivot_value)`, since grouping on the value alone
would conflate two types sharing a value space. This view is an internal administration tool and is **never exposed
to an organization**: a cross tenant count is an information leak.

**An escape hatch for exceptions.** The need to hide a pivot specific to one organization deserves its own
mechanism, otherwise it ends up in the global table. Not required for v1, to be introduced at the first request, and
applied after the global denylist.

## 10.6 Volatility as sliding daily buckets

The list wants the number of changes over seven days, and that data only exists in `observation`, which the list
may not read. Three ways out were rejected before the fourth:

- **An integer counter maintained on write.** A sliding window needs a decrement as each change expires, therefore a
  periodic sweep of the whole inventory, for a display value.
- **`last_changed_at` alone.** It says *when*, not *how many times*. Frequency is what separates an asset under
  active development from one that moved once six days ago, and only the first is a lead.
- **An exception to "no query touches `observation`".** It would open a hole in a rule protecting the hottest write
  path of the system, for a badge.

**What is kept: seven buckets, one per day, plus one.**

```sql
ALTER TABLE asset_current
  ADD COLUMN change_buckets int[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0}',
  ADD COLUMN buckets_day    date;
```

The decrement disappears because no total is stored. When recording a change, inside the `UPDATE` the ingestion
transaction already emits: if `buckets_day` is older than today, shift the array by the difference in days and pad
with zeros, then set `buckets_day` to today; then increment the first bucket. Volatility is the sum of the first
seven. No sweep, no decrement, no extra query.

**The eighth bucket is margin.** It absorbs the partial current day so the seventh is not truncated when the shift
happens.

**The shift is lazy, which handles dormant assets with no code.** An asset that has not moved in three weeks shows a
gap greater than eight, and the array is simply zeroed, which is the right answer rather than a special case.

**The trap is on the read.** The array of an asset unchanged for five days has not been shifted, since nothing
rewrote it. Naively summing the first seven buckets would count changes twelve days old as if they were yesterday's.
The sum is therefore computed relative to `buckets_day`, in a `STABLE` database function rather than an expression
copied into each caller: copied, it would be right in one place and wrong in the other.

**A first observation is not a change.** Incrementing on any non deduplicated observation counts the arrival of an
asset, which the row already carries as its age, and it counts it once **per layer**, so a freshly discovered asset
scored three or four. Since volatility is also a filter and a facet, `volatility > 2` would then return everything
just discovered, which is the opposite of the question. `last_changed_at` follows the same rule and stays NULL on an
asset that has never changed, which the console renders as "never".

## 10.7 The list is a list of hosts

The service is the unit of identity ([4.3](/architecture/data-model/#the-unit-of-a-web-asset-is-the-service-never-the-path))
and every open port becomes an asset
([8.1](/architecture/verification/#an-open-port-becomes-an-asset)). A single name therefore occupies several rows: the
fqdn, `https://`, `http://`, and one per additional open port. Five ports of one address **have** the same ASN, the
same geolocation and often the same certificate, so the repetition is a property of the model and has to be solved by
the structure of the screen rather than by a display filter.

So the host is a **header** and each service is a **row**.

**Grouping is done by the server, not by the page.** Grouping fifty already fetched assets breaks at the page
boundary: a host whose services fall on either side renders as two partial groups with two wrong counts. Pagination
is therefore over **hosts**, and the cursor is a host cursor.

**Groups are ordered by their most recent asset**, so "what moved recently" stays at the top one level up.

**A value rises into the header only if every member carries it, identically.** A host with two A records, or a
fronted service next to a direct one, would otherwise let the header assert one member's value for all of them. When
members diverge, the value drops back onto the rows, where it is true. The same rule covers lineage, which is per
asset and only folds up when the last step is the same for the whole group.

**A group of one asset stays a single row.** A header followed by one line costs twice the height for the same
information, and half the groups are typically a name that the scope has not settled and nothing has ever probed.

**Facets do not change.** They are aggregations over the filtered result, and the filtered result is still a set of
assets: "16 services on 443" stays the right answer whether the screen folds them or not. A facet counting hosts
would answer a different question, and nobody asked it.

### It is a second route, not a flag on the first

`POST /assets/hosts` beside `POST /assets/search`, taking the same filter and answering `groups` where the other
answers `assets`.

The reason is the cursor. A grouped page walks `(max(last_seen), host)` and a flat one walks
`(last_seen, asset_id)`, so the two hand out cursors that mean different things. Behind one route with a flag, a
client that flips the flag and keeps the cursor gets a walk that restarts or skips, and neither says anything. Behind
two routes the mistake is a refusal, because neither cursor decodes as the other: one is an opaque pair of
timestamp and identifier, the other is an encoded object, and a decoder that reads the wrong one fails rather than
guessing.

It also leaves the flat list exactly as [10.8](#108-export) needs it. The export walks the flat one, and an export
that had to unfold groups to write rows would be a second set of rules to keep in step with the first.

**The group cursor bounds the group, never the row.** Bounding the rows as well reads like a free narrowing and is
wrong: dropping the rows above the cursor changes what `max()` is computed from, so a host already returned comes
back with a smaller maximum and passes the bound a second time. The cost of not doing it is stated rather than
hidden, since the aggregate is then computed over the whole filtered set on every page. On the perimeters this is
built for that is a grouping over thousands of rows; the honest fix the day it stops being is a materialized per
host timestamp, not a bound that is wrong.

**A host with no value falls back to its own key.** A row whose `host` is null is grouped under its key, which shows
it alone and in its place in the order. Pooling every such row under one empty name would produce a group that means
nothing and sits at the top.

**The two statements read under the same filter.** A group on a list filtered to `status_code = 200` shows the
services that match, not the eight the host has. Reading every asset of the host instead would make the fold
disagree with the facets beside it.

### The favicon images travel on the page

Keyed by hash, once per page, beside the groups rather than on each row. A shared favicon is the interesting case
and the whole reason [10.2](#the-favicon-image-is-not-in-attributes) stores one copy, so repeating two kilobytes per
asset would undo it at the last step.

They are rendered as data URIs. An endpoint per image is a request per row from a page that has just been drawn,
and the bound that makes this safe is already in the schema: an image above 64 kB is not kept, so a page of fifty
carries what a page of fifty can carry.

### What a row carries

The guiding rule: **every displayed attribute is evidence or a pivot, ideally both.** A field that is neither does
not earn its place. The corollary is that there is no composite score, no severity on an asset, and no environment
label, which cannot be determined from the outside anyway.

| On the header | On the row |
|---|---|
| favicon, host, service count | lifecycle dot, port and scheme |
| ASN and organization, geolocation, CDN or WAF | status chain and where it landed |
| certificate, when shared | title, or the explicit mention for an `unobservable` |
| lineage, when the last step is shared | pivots that distinguish, volatility, age |

Badges whose value comes from the fingerprinter carry **their own timestamp**, distinct from the row's
`last_checked_at`. The service only runs on the five triggers of
[8.3](/architecture/verification/#83-when-a-render-happens), so those values can be significantly older than the last
verification, and the interface exposes the gap on hover.

**Script hashes are not badges.** A modern application page carries dozens of internal scripts, and a real inventory
produced 464 hash badges across 50 rows. A cap with a "+N" indicator would treat the symptom: the cause is a
**granularity error**, since a badge must fit in a line scanned in under a second, and that is incompatible with this
order of magnitude whatever the cap. A counter threshold does no better, because the counter-of-1 case is already
filtered and what remains is real sharing, so excluding it would need a threshold that is a function of program size.
The pivot stays fully functional in search and in the asset view, with its counter. Only the badge goes.

**Opening an asset in a browser** needs three attributes, and they are not stylistic precautions, because an ASM
console points at hostile pages by construction. `rel="noopener"` stops the opened page reaching `window.opener` and
redirecting the console's tab to a fake login screen, which targets exactly an operator who just clicked.
`rel="noreferrer"` keeps the console URL, filters included, out of the target's `Referer`. `target="_blank"` keeps the
console open with the state of the list. The link only appears when there is a URL to open.

### The three absences

Three things the screen deliberately does not have:

- **No result count.** A `COUNT` over the filtered set is a second full scan, and that budget is spent on facets,
  which is what people actually read. The volumes are in the side column.
- **No sort control.** One order, `last_seen DESC`, the one the cursor paginates on. A choice of sort is a second
  index and a second cursor key, for a list whose question is "what is new".
- **No textual query bar** in v1. The language comes after the AST, and filters are set through facets and badges.

### A missing cookie badge has three causes

| What the row shows | What says so |
|---|---|
| never rendered | `last_fingerprint_at` is null |
| rendered, no cookie | the timestamp is set and `attributes` has no `cookie_names` key |
| rendered, cookies that no badge deserves | the key is there and the two display filters removed everything |

The third is a site setting only `PHPSESSID`, which is common. Showing it as the second would assert that it sets no
cookie, which is false, and [10.5](#105-pivots) is explicit that the denylist removes a badge and never data.

This matters beyond the badge. An asset in the [protected regime](/architecture/verification/#86-reachability-per-observer),
the one where the HTTP probe gets nothing usable, can go a long time without a baseline, and those are frequently the
most interesting targets. The data is missing exactly where one would look for it, so the cookie badge doubles as a
fingerprint coverage indicator. **An absence of data must not read as data.**

### Three states of enrichment

A deployment without a MaxMind database is a normal deployment, and the row then shows an **empty** infrastructure
family, indistinguishable from missing data.

| State | Display |
|---|---|
| Enrichment not configured | family **not shown**, and an indicator in settings |
| Enriched, CDN asset | "CDN" in place of the geolocation, the ASN stays |
| Enriched, no match | absence shown explicitly |

The first case matters most in practice: an empty area reads as a broken interface, and someone will go looking for a
fault where there is none.

**The console cannot tell the first case from the third by looking at the data.** No asset carries an ASN in either.
So the server says it, through the endpoint that already exists for a console to discover what it may ask for rather
than learning it against 400s. Deducing it from missing data would be guessing.

### `unobservable` and `inactive` each say a sentence

`unobservable` needs an explicit mention rather than a colour code, and the useful distinction is wider than the name
of the state. "No observer gets through" is an **absence of measurement**; "the name no longer resolves" is a
**measurement**. The first licenses no conclusion about the asset, the second is one.

Hence two treatments rather than one: an `inactive` row is desaturated because there is nothing left to look at, and
an `unobservable` row keeps its contrast because nothing has been concluded on it.

## 10.8 Export

The export is the same query as the list, rendered in full. It compiles the same AST, carries the same organization
clause, and reads the same table. There is no "export query", which would be a second set of rules to keep in sync
with the first.

**It walks, it does not accumulate.** The walk reuses the list's pagination key, page by page, writing as it goes. An
`OFFSET` on a million rows costs a scan of everything before it on every page, and materializing the result before
sending it holds the whole inventory in memory for a file nobody reads until it is finished.

**Two formats, and what each gives up.** JSONL is the lossless one, one line per asset, `attributes` and lineage
included: that is the one for feeding another system. CSV is the one that opens in a spreadsheet, and it flattens.
Promoted columns, joined technologies, volatility; no `attributes`, no lineage, because a nested object in a cell is
neither readable nor usable. The loss is named here rather than discovered by someone looking for a favicon in a
spreadsheet.

**The export does not know the denylist.** The list removes a badge, never data, and an export applying a display
filter would do exactly what that rule forbids while making it invisible, since a file does not say what it does not
contain. It does not know the counter-of-1 guardrail either, for the same reason.

**No silent cap.** A limit can be asked for; it is never imposed. A truncated export that says nothing is the worst of
the three possible behaviours, ahead of the slow export and ahead of the refusal.

**The status is decided after the first page, not before it.** An export that streams cannot turn a later
failure into a status, which is understood; sending the status up front means it cannot turn the *first*
one into a status either, and then a database refusing the query arrives as `200` with an empty file. An
absence must not read as data, and a failure must not read as an absence.

## 10.9 The asset view

This is the only read path that touches `observation`, and it is worth being precise about why it may.

Principle 1 does not say nobody reads the journal. It says the interface queries `asset_current` and that observations
serve history and diff. A timeline of changes **is** history, and it is the one thing the projection cannot carry,
since by construction it keeps only a current state.

What stays true, and is the substance of the rule: the **list** and the **facets** never touch the journal. They are
what runs over a million rows and on every keystroke. A detail view opens on **one** asset, on demand, and reads an
index that already exists.

**The boundary is demonstrated from both sides.** Revoking `SELECT ON observation` from the application role, the
list, the facets and the export must **work**; the asset view must **fail**. The second is what stops the boundary
moving in silence: the day someone makes the list read the journal, the first test still passes if the privilege came
back in the meantime.

### What it renders

Six tiles first: HTTP response, certificate, network, open ports, last probe, volatility. These are the few answers
that decide whether an asset deserves the next ten minutes.

**A tile with nothing to say names which absence it carries.** "Never probed" is not "no answer", and "never rendered"
is neither. That is the rule of the three cookie states applied to a whole page rather than to one badge family.

**The network tile disappears when the deployment does not enrich.** Not empty, not greyed: absent, with the absence
stated once in the side column where it can be fixed.

**Volatility is not coloured.** Painting the number above three would fix the thresholds that
[10.6](#106-volatility-as-sliding-daily-buckets) refuses to invent for lack of several weeks of data. The counter
counts, and that is all it does.

Then one panel per layer, each read twice: once **curated**, in the vocabulary of the layer, and once **whole**, in the
fold at the bottom.

- The redirect chain is drawn as a descent, one hop per line, with each hop's destination. The row carries the codes;
  here there is room to show *where* each hop was sending.
- The certificate carries its validity window as a bar, its issuer, and its `cert_spki_hash` as a pivot with its
  counter.
- The render carries the favicon, the technologies, the cookies, and **the internal scripts as a counted list**. Twelve
  lines on one asset are readable where twelve badges across fifty rows were not. It was a granularity problem, so the
  granularity is what changes.

**The raw fold is the complete payload, and the grouping is structural.** An object or an array of objects becomes a
block, scalars gather into one block, and none of that consults a list of interesting keys. That is what keeps it on
the right side of the denylist rule: a denylist lets through whatever a producer starts emitting, while an allowlist
makes it disappear silently. A producer emitting a new key gets its block without a line of code here.

**Repetition is marked, never removed.** A header block identical to the last hop's is folded with a sentence saying
which hop it duplicates. Deleting it would produce a fold nobody could trust.

**Security headers are the one allowlist in the console**, and the justification is the inverse of the rule: the entire
point of the block is to name what is **missing**, and a denylist cannot name an absence. What it costs is written
rather than discovered: a header nobody listed does not appear in this block, it appears in the raw fold, which shows
every key. No grade, no letter, no colour that ranks. A header is a fact; a grade would be the composite score this
document refuses everywhere. An absent header is dotted grey rather than red, because nothing here is a defect: half
the services in an inventory have no reason to send a CSP.

**`observed_at` and `last_confirmed_at` are two sentences.** One is the last probe, the other is when the current state
**began**. Side by side and unnamed, they read as stale data, so each panel writes both.

**The port counts come back, never the list.** A hundred identical numbers on every asset are the probe's settings
copied per row, so normalization drops them. Their **count** is another matter: "one open out of a hundred scanned"
separates "nothing else is open" from "nothing else was tried".

**The timeline is not a list of observations.** The journal is already deduplicated on write, so two consecutive rows
of one `(asset, layer)` are two distinct states by construction: each row **is** a change, and the interval
`observed_at → last_confirmed_at` says how long it held.

**The diff shown is the Notifier's.** The timeline calls the same comparison function on the same pairs of rows.
Writing a second comparator for the screen would give an interface showing a different change from the alert received
yesterday, and that is the kind of divergence nobody notices until they have to explain which of the two is right.

**It is bounded, by two things.** An asset probed hourly for a year has thousands of rows, and a page reading them all
fails on the most interesting assets. A cap per layer, and a default window. The cap is **stated on screen** when it
cuts, for the same reason the export refuses to truncate silently.

**Retention shapes it.** Everything is kept for twelve months, then only transitions, so the timeline is dense over the
year and then thinner. That is the right answer rather than a limitation to fix, but it has to be written, otherwise
someone will read the thinning as data loss.

### What the two bounds are, and which one gets said

`GET /assets/{id}`, on the read action, answering the asset exactly as the list renders it plus the last observation
of each layer, the timeline, and the images of whatever favicon the asset carries.

The window is **ninety days** and the cap is **fifty entries per layer**. Both are constants rather than parameters,
because a caller able to ask for the whole journal of an asset probed hourly for a year is a caller able to ask for
the query this bound exists to prevent.

**Only the cap is reported, and the window never is.** The two absences are not the same absence. The cap cutting
means there is more inside the window the page is not showing, which is a fact about this render and has to be on
screen. The window is a property of the view rather than of the asset, so a page announcing it on every asset would
be announcing its own settings to somebody who did not ask. The layers that were cut are named, because "the
timeline was truncated" on a page with four panels does not say which one to distrust.

**The diff is computed here rather than stored.** The timeline calls the same comparison the Notifier calls, on the
same pairs of consecutive rows. It is the one thing on this page that costs CPU per entry, and it is the one thing
that must not be a second implementation.

**The oldest entry read for a layer carries no diff, and that is not "nothing changed".** It is the first row inside
the window, so what it moved from is outside it. The field is absent rather than empty, and the screen says "not
compared".

## 10.10 The live feed of discoveries

### Polling, not a database notification

Three sources were possible, and the most modern looking one is the most expensive exactly where this project does not
want to pay.

- **`LISTEN` / `NOTIFY`.** A `NOTIFY` in the ingestion transaction is another round trip on the hottest write path,
  and a discovery run produces thousands of assets. Two further faults: `NOTIFY` is **global**, so a subscriber
  receives every organization's channels and the filtering falls back into the control plane, and `LISTEN` **pins a
  connection** for the duration, so an open tab costs a pool connection.
- **A message channel.** The same write cost, plus a dependency on a read path that had none.
- **Polling with a cursor.** No write cost, no pinned connection between rounds, and the organization filter is the
  one every other query uses, so Row-Level Security applies with nothing special.

**It is polling**, and the argument that decides is not cost but the nature of the data: **discovery already arrives in
batches**. A run posts one report, so sub second latency describes nothing real. Paying a notification per asset to
show a batch faster than it exists would be optimizing against the producer.

### The cursor, and why `first_seen` is safe here

The feed orders on `(first_seen, asset_id)`, and `first_seen` is **mutable**, which is precisely what makes a cursor on
`last_seen` miss rows. The difference is the direction of travel: the upsert writes `first_seen = LEAST(old, new)`, so
that column can only **move backwards**. An asset moving backwards becomes older than a cursor already passed, so it is
neither re-emitted nor missed. A column that moves forward passes over the cursor and vanishes from the walk.

So the safety comes from the `LEAST`, not from the nature of the timestamp. The day something makes `first_seen` move
forward, this feed starts skipping discoveries in silence.

### Bounded, and it says so

The first run of a program produces thousands of assets, and a feed emitting all of them at the rate they arrive makes
the tab unusable at the exact moment there is most to see. The rule is the one from
[12.4](/architecture/notifications/#124-aggregation-and-anti-flood), transposed: a cap per round, and **a summary
beyond it** rather than silence. An overflow never produces an absence of information.

### What an event carries

Enough for a row, not a card. Key, kind, lifecycle, scope status, `first_seen`, the source and the last step of the
lineage, which answers "what just appeared and why". The rest is one click away.

**No query on `observation` here either.** The feed reads `asset` and `asset_current`, like the list.

Two bounds that have nothing to do with display. **The feed's duration**: a forgotten tab holds an HTTP request
indefinitely, so the stream ends on its own after a bounded time and the client reconnects, which the SSE protocol does
by itself. An eternal stream is a leak visible only in the connection count. And **a heartbeat**, because a proxy cuts a
silent connection and the browser does not say so. Resumption is free: the event id **is** the cursor, so the
`Last-Event-ID` the browser sends back on reconnection is enough, with no server side state.

### One event per tick, not one per discovery

`GET /feed`, on the read action. Each SSE message carries a **batch**: the discoveries of one round, the count the cap
left out, and the cursor as its id.

One message per asset would put the cap in the wrong place. A round that found four hundred assets would emit four
hundred messages of which the client keeps the last fifty, and the overflow the round is supposed to announce would
have no message of its own to travel in. A batch carries its own bound and says what it dropped, in one object.

**A round that found nothing emits nothing**, and the connection stays open on the heartbeat. Emitting an empty batch
per tick would advance the id on every tick, so a client reconnecting on a `Last-Event-ID` would be resuming from a
cursor that never named a discovery. The heartbeat is a comment line, which the protocol drops rather than delivering
as an event, so nothing about it can be mistaken for data.

**The cap orders by `first_seen` ascending and keeps the oldest**, which is what makes the cursor advance past them.
Keeping the newest would leave the cursor where it was and re-read the same head of the queue on the next tick, and
the feed would stall on a first run rather than draining it.

**The tenant is the connection's, and the round trip is the one every other read makes.** No `LISTEN`, so nothing
pins a connection between rounds, and Row-Level Security applies with nothing special.

