---
title: Data model
description: Assets, immutable observations and a materialized current state, plus the canonical keys that make deduplication possible.
sidebar:
  order: 4
---

## 4.1 Three layers

| Layer | Nature | Mutability |
|---|---|---|
| **Asset** | the stable identity of a piece of surface | never deleted, natural key |
| **Observation** | what a producer saw, at a point in time | immutable, append only |
| **Current state** | derived view, the last observation per asset and layer | materialized, rewritten |

Everything an ASM interface shows, the last seen date, the technologies, the ports, the CNAME chain,
is a projection of timestamped observations. The **diff** between two successive states is the
product.

## 4.2 Main tables

```sql
-- Multi-tenancy from the first migration (see Security and multi-tenancy)
CREATE TABLE org (
  id              uuid PRIMARY KEY,
  name            text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);

-- A program is one authorized perimeter
CREATE TABLE program (
  id                uuid PRIMARY KEY,
  org_id            uuid NOT NULL REFERENCES org(id),
  name              text NOT NULL,
  platform          text,                -- hackerone, yeswehack, private, ...
  platform_ref      text,                -- descriptive, never a join key, see 5.4
  authorized_from   timestamptz NOT NULL,
  authorized_to     timestamptz,         -- NULL means open ended
  authorization_ref text,                -- policy URL, contract, ...
  discovery_interval interval NOT NULL DEFAULT '7 days',
  last_discovery_at timestamptz,
  state             text NOT NULL DEFAULT 'active',  -- active | suspended | archived
  version           int NOT NULL DEFAULT 1
);

-- The scope is persistent and versioned, not a scan setting
CREATE TABLE scope_rule (
  id              uuid PRIMARY KEY,
  program_id      uuid NOT NULL REFERENCES program(id),
  kind            text NOT NULL,        -- include | exclude
  matcher         text NOT NULL,        -- apex | fqdn | cidr | regex | url_prefix
  pattern         text NOT NULL,
  valid_from      timestamptz NOT NULL DEFAULT now(),
  valid_to        timestamptz,          -- NULL means in force
  note            text,
  version         int NOT NULL DEFAULT 1
);

CREATE TABLE asset (
  id               uuid PRIMARY KEY,
  org_id           uuid NOT NULL,
  program_id       uuid NOT NULL REFERENCES program(id),
  kind             text NOT NULL,       -- fqdn | ip | service | url
  key              text NOT NULL,       -- canonical form, see 4.3
  parent_asset_id  uuid REFERENCES asset(id),
  discovery_source text NOT NULL,       -- fastrecon:crt | certstream | portscan | manual | ...
  discovery_path   jsonb,               -- lineage, see 4.4
  scope_status     text NOT NULL,       -- in_scope | out_of_scope | unknown
  first_seen       timestamptz NOT NULL,
  last_seen        timestamptz NOT NULL,
  UNIQUE (program_id, kind, key)
);

-- Immutable journal, deduplicated on write and partitioned, see 4.5
CREATE TABLE observation (
  id                bigserial,
  asset_id          uuid NOT NULL REFERENCES asset(id),
  observed_at       timestamptz NOT NULL,  -- when this state began
  last_confirmed_at timestamptz NOT NULL,  -- last time it was seen unchanged
  run_id            uuid,                  -- the run that produced it, see 9.1
  source            text NOT NULL,         -- fastrecon | fingerprinter | manual
  layer             text NOT NULL,         -- dns | tcp | http | fingerprint
  outcome           text NOT NULL,         -- ok | fail | error, see 6.4
  producer_version      text,              -- instrument version at the first pass
  last_producer_version text,              -- version at the last confirmation, see 8.7
  data              jsonb NOT NULL,
  PRIMARY KEY (id, observed_at)
) PARTITION BY RANGE (observed_at);
CREATE INDEX ON observation (asset_id, layer, observed_at DESC);

-- Per layer verdict and failure counters. One row per layer that has an opinion.
CREATE TABLE asset_layer (
  asset_id                 uuid NOT NULL REFERENCES asset(id),
  layer                    text NOT NULL,
  state                    text NOT NULL,   -- see 6.1
  informative_failures     int NOT NULL DEFAULT 0,
  non_informative_failures int NOT NULL DEFAULT 0,
  first_failure_at         timestamptz,     -- carries the 24 h floor of 6.5
  last_ok_at               timestamptz,
  last_checked_at          timestamptz,
  PRIMARY KEY (asset_id, layer)
);

-- The materialized view. This is what the search API reads.
CREATE TABLE asset_current (
  asset_id        uuid PRIMARY KEY REFERENCES asset(id),
  org_id          uuid NOT NULL,
  program_id      uuid NOT NULL,
  kind            text NOT NULL,
  key             text NOT NULL,
  scope_status    text NOT NULL,

  -- layer verdicts, projected from asset_layer
  dns_state       text,     -- resolving | nxdomain | nodata | wildcard | unknown
  tcp_state       text,     -- open | closed | filtered | unknown
  http_state      text,     -- responding | error | unreachable | unknown
  lifecycle       text NOT NULL,  -- candidate|active|flapping|inactive|unobservable|archived

  -- scheduling, see 6.3
  next_resolve_at     timestamptz,
  next_full_at        timestamptz,
  next_fingerprint_at timestamptz,
  -- low = 100, high = 50. Sorted before the due date, so a change detected
  -- after a mass baseline does not queue behind it (see 8.3).
  fingerprint_priority smallint NOT NULL DEFAULT 100,
  backoff_tier        int NOT NULL DEFAULT 0,

  -- which observer gets a result on this target, see 8.6
  http_reachable        boolean,
  fingerprint_reachable boolean,
  http_streak           int NOT NULL DEFAULT 0,  -- signed: successes > 0, failures < 0
  fingerprint_streak    int NOT NULL DEFAULT 0,

  -- promoted columns, the ones filtering is done on
  host            text,
  port            int,
  scheme          text,
  ip              inet,
  status_code     int,
  status_chain    int[],
  final_url       text,
  title           text,
  server          text,
  asn             int,      -- Geo-IP derivatives, computed at ingestion, see 8.8
  asn_org         text,
  country         char(2),
  region          text,
  city            text,
  is_cdn          boolean,
  cdn_provider    text,
  waf_detected    boolean,
  waf_vendor      text,
  technologies    text[],

  -- volatility, see 10.5
  change_buckets  int[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0}',
  buckets_day     date,

  attributes      jsonb,

  first_seen      timestamptz NOT NULL,
  last_seen       timestamptz NOT NULL,
  last_checked_at timestamptz,
  last_fingerprint_at timestamptz,
  last_ok_at      timestamptz,
  last_changed_at timestamptz
);

-- What Certificate Transparency has actually delivered under one apex, see 7.5.
-- Counters rather than a flag on the program: a program holds several apexes and
-- they do not behave alike, and a boolean would never expire.
CREATE TABLE ct_apex (
  org_id           uuid NOT NULL REFERENCES org(id),
  program_id       uuid NOT NULL REFERENCES program(id),
  apex             text NOT NULL,
  watched_since    timestamptz NOT NULL,  -- so a young apex is not read as a silent one
  san_count        bigint NOT NULL DEFAULT 0,
  wildcard_count   bigint NOT NULL DEFAULT 0,
  last_san_at      timestamptz,
  last_wildcard_at timestamptz,
  PRIMARY KEY (program_id, apex)
);
```

`ct_apex` carries no coverage score. The reading is
[derived from these counters](/architecture/discovery/#wildcard-certificates-and-the-metric-that-follows)
at query time, because a stored score is a number nobody can recompute the day its formula changes, and
this one is computed on data nobody has yet.

Indexes: GIN on `attributes` and `technologies`, B-tree on the promoted columns, a composite
`(org_id, program_id, lifecycle)`, and one expression index on `reverse(key)` for the suffix query
of [10.1](/architecture/search/#the-suffix-is-the-query-that-matters).

`waf_source` stays inside `attributes`. It is traceability, and nobody filters on it.

The identity tables (`app_user`, `membership`, `api_token`) and the attribution columns are defined
in [11.1](/architecture/security/#111-irreversible-decisions). The `run` table is in
[9.2](/architecture/deployment/#92-a-run-has-a-row).

## 4.3 Canonical keys

Deduplication depends entirely on normalization.

| Kind | Canonical form | Example |
|---|---|---|
| `fqdn` | lowercase, trailing dot removed, IDN to punycode | `api.target.com` |
| `ip` | normalized form, IPv6 compressed | `2606:4700::1` |
| `service` | `host:port/proto` | `api.target.com:443/tcp` |
| `url` | scheme, host, port when non standard, normalized path. Query and fragment removed | `https://api.target.com/v1/users` |

**The query string is excluded from the key.** Placeholders such as `?id=FUZZ` do not fix it: the
order of parameters is arbitrary, and optional parameters produce several keys for one endpoint. The
deeper reason is that the inventory is about **surfaces**, not requests. `https://app.target.com/admin`
is the asset; `?page=2` is not a second one.

Path normalization decodes redundant `%XX` sequences, collapses repeated slashes, resolves `.` and
`..`, keeps case (paths are case sensitive, hosts are not), and strips a trailing slash except at the
root.

Three details the implementation forces:

- **A `%XX` of a reserved character is not decoded.** `%2F` is a slash *inside* a segment, not a
  separator, and decoding it changes the path. Only unreserved characters are decoded, and the hex
  case of what remains is uniformized.
- **Underscore labels are accepted.** `_dmarc.target.com` and `_domainkey.target.com` are everywhere,
  and a strict hostname grammar would drop real assets.
- **An IPv4-mapped address is reduced to its IPv4 form.** `::ffff:1.2.3.4` and `1.2.3.4` are one
  machine, not two assets.

The query survives in `attributes`, where `query_params_seen` has value of its own: a `debug` or
`admin` parameter appearing is a signal.

### The unit of a web asset is the service, never the path

A path is where a redirect landed that day. An application that renames its login page would retire
one asset and create another, and the new `first_seen` would announce a surface that was always
there. The identity would have moved while the target did not.

**Rule: no producer creates an asset of kind `url`.** An observed URL is recorded on the **service**
it belongs to, and its path stays in the observation payload. An HTTP response seen on a path is not
recorded at all: it does not measure the service, and endpoints are not a table in this model.

The `url` kind stays legal for one case, what a human declares. The `url_prefix` matcher of
[`scope_rule`](#42-main-tables) exists to frame a path, and a program whose perimeter is
`https://target.com/api/` has to be able to name it. Identity by path is an act, never a byproduct
of a scan.

### What the key contains is filled at creation

Any promoted column **derivable from the canonical key** is written when the asset row is created,
never derived from an observation. There is nothing to wait for: the information is in the key at
the moment the row is written.

This covers `host`, `port` and, for a `url`, `scheme`. Deriving them in SQL later would be a second
implementation of key parsing, and `split_part(key, ':', 1)` is wrong on the first IPv6 key, which
reads `[2606:4700::1]:443/tcp`.

Only columns that describe an **observed state** wait for an observation: `status_code`, `title`,
`technologies`, `is_cdn`, `server`, and the three `*_state` columns.

## 4.4 Lineage

Knowing **why** an asset is in the inventory matters for debugging, for trust, and for justifying a
scan to whoever owns the target. `asset.discovery_source` names the mechanism and
`asset.discovery_path` carries the steps:

```json
[
  {"step": "enumerated", "run": "01M0GH...", "sources": ["crt", "chaos"]},
  {"step": "resolved",   "run": "01M0GH...", "addresses": ["93.184.216.34"]},
  {"step": "port_open",  "run": "01M0GJ...", "port": 8080}
]
```

The sources come from `hosts[].sources`, sorted so that two runs of the same perimeter compare equal.
A set coming back in a different order each time would defeat the deduplication of 4.5 on exactly the
assets it protects.

`ON CONFLICT` never touches `discovery_source`, so its value is that of the first appearance. That is
the question it answers.

## 4.5 Observations

Order of magnitude: 50 000 assets, a few probes a day, more than 95 % of them identical to the one
before.

### Deduplication on write

An observation is inserted only when it differs from the last one of the same
`(asset_id, layer)`. One tool produces the first three layers, so a layer has one chain and one
normalization ([7.1](/architecture/discovery/#why-there-is-no-documentary-observation)):

```
if outcome and data are identical to the previous observation:
    UPDATE last_confirmed_at
else:
    INSERT
```

Each row then means "this state held from `observed_at` to `last_confirmed_at`". Volume drops by
more than an order of magnitude **without losing information**: the state at any date is still
reconstructible. A useful side effect is that two consecutive rows are two distinct states by
construction, which makes the [diff](/architecture/notifications/#121-structured-diffs) trivial.

The comparison covers `outcome` and `data`, **not the producer version**. A version bump that changes
nothing in the result must not write a row, which is why the version is stored twice
([8.7](/architecture/verification/#87-dating-the-instrument)).

### Normalization comes first

The comparison only means something on normalized structures. Without it, deduplication deduplicates
nothing and the volume stays at its raw order of magnitude while no error is ever raised.

**One place, one function.** A single `normalize(layer, data)`, applied before every write to
`observation`. No write path goes around it, because a normalization reimplemented per producer will
diverge and nothing will say so.

What it does, in every layer: sort arrays whose order carries nothing, uniformize case on case
insensitive identifiers and never on paths, parse versions to a canonical form, and remove fields
that change on every pass by construction.

Per layer, the fields that are dropped and why:

| Layer | Dropped | Reason |
|---|---|---|
| `dns` | nothing volatile arrives | FastRecon reports no TTL, which is the field that would otherwise defeat this layer on its own |
| `tcp` | the scanned, closed and filtered port lists | a hundred identical numbers on every asset are the probe's settings copied per row. Their **counts** are kept, because "one open out of a hundred scanned" separates "nothing else is open" from "nothing else was tried" |
| `http` | `response_time_ms`, and `content_length` by default | both measure the request that just happened. A page carrying a CSRF token or a session id differs on every pass, and `http` is the busiest layer of the system |
| `fingerprint` | `screenshot`, `scanned_at`, cookie values, `network.ips`, `chain[].remote_ip_address`, the root `version` | see below |

The `fingerprint` line is the long one, and each entry has its own reason.

- **Cookie values** are session identifiers reissued on every scan. The names survive, because a name
  is a [pivot](/architecture/search/#105-pivots). This is a deliberate loss: a diff on a
  non-volatile application cookie would have meaning, but nothing reliably tells such a cookie from a
  `PHPSESSID`, and being wrong in that direction costs the whole layer's deduplication.
- **Addresses** belong to the `dns` layer, which resolved them once. A renderer that resolves the
  same name again is a second producer for a value it does not own, and the two cannot agree on a geo
  balanced or fronted name.
- **The root `version`** is promoted into `observation.producer_version`, a column the deduplication
  deliberately does not compare. Leaving it in `data` would smuggle it back into the comparison, and
  every version bump would rewrite the payload of the entire inventory at once. The same name one
  level down, inside `technologies[]`, is the version of a detected product: it stays, and it is
  canonicalized.

Headers reach the model through the `fingerprint` layer only, and get three treatments.

**Removed** for measuring the request rather than the service: `date`, `age`, `content-length`,
`expires`, `connection`, `keep-alive`, `etag`, `last-modified`, `retry-after`.

**Emptied, name kept**, when the presence is evidence and the value is noise. `CF-RAY` is a
[CDN signature](/architecture/verification/#86-reachability-per-observer) and a request id at the
same time; dropping it to be rid of the value would throw away the signal with the noise. The generic
rule covers what no one has listed yet: any header whose name contains `-id`, `-trace`, `-request`,
`-ray`, or a duration marker such as `-time`, `-timing`, `runtime`, `-duration`, `-latency`, is
emptied by default, with a short exception list.

**Sorted when order says nothing.** A header sent on several lines is a set, and two responses listing
the same values in a different order are the same response. The exception is hop chains, `Via`,
`Forwarded`, `X-Forwarded-*`, `Server-Timing`, `Trailer`, which read from the first relay to the last.
Sorting them would claim the request took another route.

Two special cases are worth naming because they cost a whole layer's deduplication when missed.

**The CSP nonce.** `Content-Security-Policy` is one of the most informative fingerprints of a page,
naming CDNs, analytics and permitted ancestors, and it carries a `'nonce-...'` regenerated on every
response. The header is not emptied: the nonce alone is replaced by a constant. Replacing rather than
removing is deliberate, since `script-src 'self' 'nonce-'` and `script-src 'self'` are two different
policies.

**Randomly named cookies.** The whole point of keeping cookie names is that a name identifies an
application. Some applications generate the name itself per session. Such a name is rejected at
normalization, on the entropy of the name, for the reason that defines a pivot: it cannot link any
asset to any other. `PHPSESSID` is useless because it is everywhere, a random name because it is
nowhere twice.

### Versions

`1.24` is **not** promoted to `1.24.0`. A target can legitimately distinguish the two, and the
canonical form asked for here is not invented precision. What is normalized is the case, the extra
whitespace, a leading `v`, and the separator, so that `nginx/1.24` and `nginx 1.24` converge.

A diff where one value is a **prefix** of the other is classified as a variation in detection
precision rather than an application change. Recorded, not notified. **This only holds on a field
that carries a version**: a SAN going from `example.com` to `example.com.attacker.test` satisfies the
same test and is the opposite of harmless.

### Declaring a schema per layer

The deduplication rate is an **aggregate**, so it does not catch a rule that silently stopped
applying: one unnormalized field on 5 % of observations never moves the global number. Strict
rejection on an unknown field is the other extreme, and it is too rigid: the
[Fingerprinter](/architecture/verification/#82-the-fingerprinter) ships on its own cycle and its
updates add fields, so every release would become an ingestion outage.

Hence an asymmetric behaviour:

| Situation | Treatment |
|---|---|
| Declared field, present | normalized |
| Declared optional field, absent | accepted |
| Declared **required** field, absent | **rejected** |
| **Undeclared** field | accepted and stored, counter `unknown_field{layer, name}` incremented |

The counter is exposed as an **alerted metric**, not a log line: a `techs` emitted instead of
`technologies` shows up immediately, by name, without interrupting ingestion.

:::caution[The trap that hides here]
PostgreSQL's `jsonb` already sorts and deduplicates object keys on write, so `{"a":1,"b":2}` and
`{"b":2,"a":1}` are natively equal. That behaviour does **not** extend to array element order.
Deduplication will therefore appear to work on flat structures and fail exactly on `technologies`,
`scripts` and `redirects`, which are the fields carrying the volume.
:::

**Guarantee against regression.** Expose a deduplication rate, observations deduplicated over
observations submitted, expected above 90 %. A drop signals a regressed normalization or a producer
emitting an unfiltered volatile field. Without the metric, the failure stays silent until performance
degrades.

### Retention

| Age | Policy |
|---|---|
| 0 to 12 months | everything kept |
| over 12 months | transitions kept, repeated `fail` outcomes on archived assets purged |
| never purged | `first_seen`, `last_seen`, the observation that created the asset |

Change history is never aggregated: it is the value of the product.

### Partitioning, from the first migration

Monthly partitions on `observed_at`. Purging becomes an instant `DROP TABLE` instead of a `DELETE`
that locks the table and leaves gigabytes to `VACUUM`. Cost today, five lines. Cost of retrofitting
onto tens of millions of rows, high.

PostgreSQL requires the partition key to be part of the primary key, hence `PRIMARY KEY (id, observed_at)`.

**No `DEFAULT` partition.** A row whose monthly partition is missing must fail loudly: that is the
only signal that the creation mechanism has broken. A default partition would absorb it silently, and
would have to be scanned on every subsequent attach. The maintenance job therefore works **two months
ahead**, not one, so that an incident at the end of a month does not interrupt ingestion.
