---
title: Discovery
description: FastRecon as the scanning engine, its two inputs, the curated port list, and the Certificate Transparency stream.
sidebar:
  order: 7
---

## 7.1 FastRecon is the engine

[FastRecon](https://github.com/JoshuaMart/FastRecon) takes a perimeter and returns one JSON report:
the hosts that answer, the ports they expose, and the HTTP services behind them. Five stages on a
ladder, where each rung runs the ones below it:

```
 [1] ENUMERATE ──► raw subdomains, multi-source, deduplicated
 [2] EXCLUDE ────► in-scope subdomains
 [3] RESOLVE ────► live hosts with A/AAAA/CNAME, plus dead hosts with a reason
 [4] PORTSCAN ───► open ports per live host
 [5] HTTP PROBE ─► HTTP services with the right scheme, status, title, technologies, TLS
```

What Recon gets for free and would be unreasonable to rewrite: the passive source set with its per
source accounting, mandatory wildcard DNS detection, a validated resolver pool, unprivileged connect
scanning, CDN determination per address, and scheme detection that tries TLS first because the
handshake is the only reliable discriminator.

Three properties of the report shape the ingestion path:

- **Dead hosts stay in.** A dangling CNAME is a finding, not noise, and `nxdomain` and `no_answer` are
  distinct values.
- **Every source appears**, successful or not. A source that silently returns nothing is exactly what
  that accounting exists to expose, and it is what lets Recon apply [P2](/architecture/principles/)
  rather than guess.
- **A truncated run is still a valid report.** Hosts the run did not reach come back as `discovered`,
  so a gap is stated rather than mistaken for an absence.

### Two inputs, two mandates

The same engine answers both questions of [P1](/architecture/principles/), and the difference is the
input:

| | Discovery run | Verification run |
|---|---|---|
| Input | a program apex, plus the exclusion patterns | the hosts selected by [6.3](/architecture/lifecycle/#63-scheduling-and-backoff) |
| Stage 1 | queries the passive sources | replaced by the target list |
| Scope | `full`, or `enum` for a passive pass | `resolve` or `full`, per due date |
| May conclude | that an asset **exists** | that an asset **no longer answers** |

### Why there is no documentary observation

One tool produces the `dns`, `tcp` and `http` layers, with one normalization, whichever question the
run was asked. A discovery report and a verification report are the same measurement over different
hosts, so they deduplicate against each other correctly and neither poisons the other's chain.

That is worth stating because it is not free elsewhere. A second producer on the same layer would mean
a second normalization, and a baseline written by one followed by a probe from the other would produce
a false change on every asset at the second pass. The [fingerprinter](/architecture/verification/#82-the-fingerprinter)
is a different producer, which is why it is a different layer.

## 7.2 How a verification run gets its targets

The host list replaces stage 1 rather than skipping it, so exclusions still run on its output. That is
what Recon wants: a rule may have changed between the run being defined and the run starting, and the
patterns are a second safety net in front of the network.

| Setting | Use |
|---|---|
| `--targets` | inline hosts, for a single host run |
| `--targets-file` | one host per line, for local use |
| `--targets-url` | https, fetched at startup. The one a job uses |
| `--targets-header` | headers for that fetch, so the credential stays out of anything that logs a URL |

`--domain` becomes optional and purely informational in this mode: it labels the report so Recon can
correlate, and nothing else reads it. In particular the root domain filter does not apply, which is what
lets one list span several apexes of the same program.

**The report says which input was used.** `run.input` is `domain` or `targets`, and Recon reads it to
decide what a missing host means. Without it, an empty `sources` block would be the only clue, and a
clue is not a contract.

### What the list may contain, and why Recon satisfies it by construction

Bare hostnames, normalized, one per line. No port, because the port selection is a run level setting and
a target carrying one would make it ambiguous. No address literal with colons, so an IPv6 `ip` asset has
no place in a list.

That last point decides something in [6.3](/architecture/lifecycle/#scheduling-is-per-host): an `ip`
asset is **observed through the hosts that resolve to it**, never scheduled on its own. It is not a
restriction Recon has to work around, since an address only ever enters this inventory as the answer to a
name.

Three refusals matter more than they look, and all three protect the same property:

- **An oversized list fails the run** rather than being truncated. A silently shortened list turns hosts
  that were never queried into hosts that did not answer, which is the exact false death this whole
  input exists to prevent.
- **A malformed entry fails the run**, named, rather than being skipped. A host dropped for a formatting
  reason is a host that is never queried, and nothing downstream would say so.
- **An empty list refuses to start.** A run with nothing to scan is a configuration mistake, and an empty
  report hides it.

Recon meets all three without effort. What it serves are the canonical keys of `fqdn` assets, already
normalized by [its own canonicalization](/architecture/data-model/#43-canonical-keys), so they are well
formed by construction, and the other kinds never appear in a list.

## 7.3 Source credentials

Two of the default sources work without a key, which is what makes a first run return data. The keyed
ones report themselves as `skipped_no_key` rather than being silently dropped.

That distinction is the whole point, because the real failure mode does not look like a failure. Without
a credential a keyed source disables itself, the run starts, finishes correctly, and simply finds less.
Nobody notices while looking at an inventory they have never seen otherwise.

**Keys live in the run's environment, never in the run definition.** The control plane **names**
secrets, it does not carry them. A key is an infrastructure credential, the same for every program,
that says nothing about the target, unlike the perimeter, the budget and the port list, which are
properties of the program being scanned. Sending it down through the run definition would put it in the
database and in an HTTP response, for nothing.

Three consequences:

- **An empty variable is an error**, not "no key". The most likely mistake, a variable declared in the
  deployment and never filled, otherwise reproduces exactly the silent run this rule exists to remove,
  while giving the impression that the key is configured.
- **A run with no key at all is valid.** It is the default configuration, and the run says so in one
  line at startup.
- **Names are logged, values never are.** Startup emits which sources have a key and which do not, which
  is the observable counterpart of a source disarming itself.

## 7.4 The port list

The port list is data, which is what [P5](/architecture/principles/) requires of anything of this kind.
It lives in Recon's configuration and travels in the run definition, so discovery and verification scan
the same ports. A second list configured on the scanner would be a second list to keep in agreement,
and nothing would raise an error when the two diverged.

It is not nmap's top 100. That ranking orders ports by how often they are open across the whole
internet, so it leads with telnet, FTP, SMTP, POP3, IMAP, NetBIOS and printing. Open everywhere,
interesting almost nowhere. A mail server answering on 25 is a mail server, and it will still be one
next month.

The criterion here is different: **a port earns its place if it can carry a finding.** Four families:

| Family | Examples |
|---|---|
| Web entry points | 80, 443, 8080, 8443, 8000, 8090, 9000, 9443, 3000, 5000 |
| Application servers and admin consoles | 2375 and 2376, 6443 and 10250, 15672, 5601, 8983, 9990, 2083 and 2087 |
| Databases and stores | 3306, 5432, 27017, 6379, 9200, 11211, 9042, 2379 |
| Remote access and open proxies | 3389, 5900, 1080, 3128 |

A forgotten Jenkins on 8090, a Docker API on 2375, an Elasticsearch on 9200. Each of the last three
families is a finding by the sole fact of being reachable from the internet.

## 7.5 Certificate Transparency

Every certificate issued for a name is published to public logs within hours, and anybody may read them.
That is the freshness advantage the [vision](/architecture/vision/) names: a staging host gets a
certificate before it gets traffic, and a platform watching the stream sees the name the same minute.

### The feed is a component, the matcher is not

Two things are easy to fold into one. `certstream-server-go` is an **aggregator**: it follows the CT logs,
normalizes what they carry and exposes it as a websocket. It is somebody else's project, it holds nothing
of Recon's, and it is deployed as an image the way FastRecon is.

The **matcher** is Recon's, and it is a loop in the control plane rather than a service of its own.

| | Aggregator | Matcher |
|---|---|---|
| What it is | `certstream-server-go`, an image | a loop in the control plane |
| What it knows | the CT logs | the apex set, and the inventory behind it |
| Credential | none | the database, like every other loop |
| Direction | dialled by the matcher, dials the CT logs | dials out, never listens |

A second service would need one of two things, and both are worse than the loop. A database credential
outside the control plane, which [9.4](/architecture/deployment/#94-separating-privilege-not-just-load)
spends its length removing. Or an ingestion endpoint that creates assets, which
[5.5](/architecture/scope/#entering-an-asset-by-hand) already placed under the scope action rather than
the one a scanner holds, precisely so that nothing holding a run's credential can widen a perimeter.

Its network position is the Fingerprinter's, for a weaker reason and with the same shape: it has no
business reaching the database, the control plane dials it and it never dials back
([8.5](/architecture/verification/#85-network-isolation)).

**The aggregator can lie, and it changes nothing.** A hostile or compromised feed can inject SANs, and the
worst it obtains is a candidate under an apex the customer already authorized, which resolves to nothing
and is archived by its own budget ([6.6](/architecture/lifecycle/#66-an-asset-that-was-never-alive)). So
Recon validates no signature and no SCT. Validating one would prove that a certificate exists, and that is
not the question being asked. The question is whether a name sits under an apex, and the answer is local.
This is [P6](/architecture/principles/) applied to a feed rather than to a scanner.

### What a frame actually carries

Transcribed from the running image rather than from its README, and pinned in
`internal/ct/testdata/stream.jsonl`, so a change on their side shows up in a decode here first. That is
the habit [4.5](/architecture/data-model/#45-observations) already imposes on the scanner's report, for
the reason a transcribed contract always eventually needs: the position of a field only exists once a
real document has been decoded.

Three endpoints, and Recon dials the middle one:

| Endpoint | Carries | Verdict |
|---|---|---|
| `/full-stream` | the lite document plus the DER and the chain | nothing here parses a certificate |
| `/` | the SANs, the issuer, the log and the index | **what Recon dials** |
| `/domains-only` | the SANs, and nothing else | cheaper on the wire, and it loses the lineage |

```
message_type                "certificate_update"
data.update_type            "PrecertLogEntry" | "X509LogEntry"
data.leaf_cert.all_domains  the SAN list, wildcards included, DNS names only
data.leaf_cert.issuer       CN and O, which is half of a candidate's lineage
data.source.name, .url      which log it arrived from, the other half
data.cert_index             its index in that log
data.seen                   when the aggregator saw it
```

**`/domains-only` was measured and refused.** It is roughly fifteen times smaller on the wire, which
would decide the question if decoding were the constraint, and the measurement at the end of this section
says it is not by two orders of magnitude. What it drops is the issuer and the log, which is exactly the
"why is this here" a candidate's [lineage](/architecture/data-model/#44-lineage) exists to record. Paying
nothing to save nothing is still paying.

### What the set holds

The set is built from `scope_rule`: **include rules whose matcher is `apex`**, in force, on programs that
are `active` and inside their authorization window.

**`fqdn` rules stay out**, and their absence is the rule of
[5.5](/architecture/scope/#an-include-that-names-a-thing-declares-it) working. An include naming a host
already declared it, so the asset exists and carries a due date. CT would find the same name and create
nothing, and putting it in the set would buy a lookup per certificate for a row that is already there.
`cidr` and `regex` never named a thing at all.

**The authorization window is in the set, not only `state`.** It is the same two lines the
[discovery cadence](/architecture/deployment/#the-pass-for-discovery) carries, for the same reason one
level earlier: an expired program left in the set keeps creating assets with due dates on a perimeter
nobody may scan, so the first thing each one does is have its run refused.

**An apex maps to a list, never to one program.** Two programs may legitimately hold the same apex, and
[9.8](/architecture/deployment/#the-pass-on-due-dates) already says the same name held by two of them is
two assets and two runs. One SAN therefore creates one candidate per program that claims it.

### The reload is a swap, on a short timer

The set is rebuilt whole and swapped, never mutated in place: the stream reads it without a lock, and a
map being edited under a walk is the one bug in this loop that would produce wrong matches rather than an
error.

The query behind it walks every tenant's rules by construction, so it runs on the system pool and is
annotated `cross-org` with that as its reason
([11.1](/architecture/security/#111-irreversible-decisions)). It is the only part of this loop that does.
Writing a candidate is scoped, on the application pool, with the organization the matched apex named.

The interval is minutes rather than an hour, because the cost is one query over `scope_rule` and the
thing it buys is that an apex added in the console starts producing candidates while the person who added
it is still looking at the screen.

### Matching walks labels, never a string

CT carries several million certificates a day. For each SAN, walk the labels up through the set:

```
san = "staging.api.target.com"
→ test "staging.api.target.com"
→ test "api.target.com"
→ test "target.com"          ← match
```

Four lookups at most, whatever the number of programs.

**The walk does not stop at the first match.** An apex may sit under another program's apex, and stopping
would silently drop the outer one. It collects every apex on the path, which is the same rule as two
programs holding the same apex, reached from the other end.

**Labels are also what makes the walk safe.** `target.com.evil.com` matches nothing, because the walk
climbs label boundaries rather than testing a string suffix. That is the same distinction
[10.3](/architecture/search/#the-suffix-is-the-query-that-matters) draws where it refuses to turn a
suffix search into a notion of domain membership, and here it is load bearing rather than conservative: a
suffix test would let anybody put a name into somebody else's perimeter by registering a domain.

### What a match creates, and what it does not

An `fqdn` asset, `discovery_source = certstream`, its lineage carrying the issuer and the log entry, and
`scope_status` from the ordinary [re-evaluation](/architecture/scope/#52-re-evaluated-at-ingestion) rather
than from a filter here. **CT classifies, it does not filter**: a name matching an apex and caught by an
exclusion is stored and never probed, like every other out of scope asset. Filtering it in the loop would
be a second scope engine, disagreeing with the first at some point.

Three kinds of SAN create nothing:

- **A wildcard.** `*.target.com` names no host. It feeds the counters below instead.
- **A certificate with no DNS name at all.** The aggregator strips the non-DNS SANs itself, so an
  IP-only certificate arrives as an **empty** `all_domains` rather than as something to refuse: the field
  is present, the list is empty, and the subject's `CN` is empty too. It is 0.8 % of the stream, and it is
  the case a decoder written from the README gets wrong first.
- **A name that is already an asset.** The insert conflicts and does nothing, which is the deduplication
  the next section is careful not to claim for itself.

The lifecycle is `CANDIDATE` and the due date is immediate, which
[6.3](/architecture/lifecycle/#who-fills-the-due-dates) already wrote down.

### The deduplication cache is a cost control, never the correctness one

`UNIQUE (program_id, kind, key)` is what makes a SAN one asset. It holds with the cache cold, with the
cache full and with the cache deleted, and the milestone that says ten sightings in a minute create one
asset is an assertion about that constraint.

What the cache buys is the round trip, and measuring the feed narrowed what that is worth. **Only a SAN
that matched an apex ever reaches the database**, so the baseline load is not the stream: it is a handful
of names an hour on a small deployment, and a cache saves a tenth of a handful. The cache earns its place
on the **burst** instead, which is the case that actually exists: an ACME loop reissuing across a large
perimeter, a wildcard-heavy apex, and the replay an aggregator restart produces if its recovery option is
ever turned on. Sizing it against the stream would have been sizing it against the wrong number.

Exact duplicates are the normal case rather than the edge one, and the dominant cause is not the one to
name first. Over four thousand consecutive certificates, 9.6 % repeat a SAN set already seen, and most of
those are the same entry reaching **several logs**. A cache built around the precertificate followed by
its certificate, which is the mechanism everybody reaches for, would have caught sixty six of the three
hundred and eighty two.

- Keyed on **(program_id, san)**, because the same name under two programs is two assets.
- Fixed capacity with a TTL in minutes, since an unbounded map on a stream is a leak with a slow fuse.
- **An eviction is not an event and nothing logs one.** The row is already in the database; the next
  sighting pays one insert that does nothing.

Saying which of the two carries the guarantee is worth the paragraph, because a cache that is load bearing
on correctness is a cache nobody may ever resize, and nobody finds that out until they resize it.

### A ceiling per program, and it says what it dropped

A perimeter can be pointed at an apex under which the stream carries thousands of names, and that is one
wrong `apex` rule away rather than hypothetical. Without a bound, a public feed decides the size of a
customer's inventory.

Candidate creation is therefore capped per program per window. Past the cap the loop creates nothing more
for that program until the window rolls, and it **says how many it did not create**. A silent cap reads as
"CT found forty names under this apex" where the truth is "CT found four thousand and these are the first
forty", which is the same failure the live feed already refuses when it
[says what it left out](/architecture/search/#bounded-and-it-says-so).

### A reconnection is a gap, and nothing backfills it

The aggregator keeps no history on Recon's behalf. A dropped websocket loses whatever passed while it was
down, and reconnecting resumes at the present.

Two consequences, both stated rather than worked around. The loop **records when it was connected**, so a
hole is visible as a hole rather than as a period during which CT found nothing, which matters because the
counters below would otherwise read an outage as an apex the logs are silent on. And the hole is covered
by the thing that was never redundant with CT anyway: periodic enumeration walks the same perimeter on the
program's own interval.

### Wildcard certificates, and the metric that follows

**A structural blind spot.** A certificate issued for `*.target.com` reveals **no** subdomain, and mature
organizations use them for exactly that. Add internal names, non TLS services and private CAs. CT and
periodic enumeration are therefore not redundant. They are complementary by construction.

A wildcard passing for an apex in the database is a signal in itself: it says CT will help little on this
program and that effort belongs in DNS brute force instead.

**The signal is per apex and dated, not a boolean on the program.** A program holds several apexes and
they do not behave alike, one served entirely by a wildcard and another issuing a certificate per host. A
flag on the program loses which apex, and a boolean never expires: an organization that stops using
wildcards would carry it for good.

So the state is counters per apex, in [`ct_apex`](/architecture/data-model/#42-main-tables), upserted by
the loop: how many names CT contributed and when the last one arrived, how many wildcards matched and when
the last one did, and since when the apex has been watched, so that a young apex is not read as a silent
one.

**Coverage confidence is derived at read time and never stored.** It is the first number in this project
computed on data nobody has yet, so its formula will change, and a stored score is a number nobody can
recompute the day it does. The counters are the fact, the reading is a view over them.

:::note[The one counter here that may lose a minute]
The counters are accumulated in memory and flushed on the reload tick, because a write per certificate is
exactly the round trip the cache above exists to remove. A crash loses the unflushed window.

That is acceptable here and nowhere else in this system. [P3](/architecture/principles/) protects the
journal, and this is not the journal: it is a metric about coverage. **An asset created from a certificate
is written immediately** and is never in that window, which is the half that would not have been
acceptable to lose.
:::

:::note[Measured against the running feed, 24 August 2026]
`certstream-server-go` v1.9.0 in the local stack, following fifteen logs, sampled over four thousand
consecutive certificates.

**The matcher has roughly seventy times the headroom it needs, and that was measured before it was
written.** Decoding one frame and walking every SAN of it through the set costs **6.8 µs** on a single
goroutine, which is 146 000 frames per second and 227 MB/s of JSON. The feed delivers about 2 000 a second
at steady state and around 4 000 while it catches up. So the milestone's single core is a question of
margin rather than of fit, and the cheap endpoint that existed to protect it is not needed.

**Wildcards are 22.8 % of SANs**, and 102 certificates out of 4 000 carry nothing else. The
[blind spot below](#wildcard-certificates-and-the-metric-that-follows) is the common case rather than a
corner of one, which is the whole argument for counting it per apex.

**A SAN list is short, and so is its tail.** Median one name, p99 six, widest 89. The burst one
certificate can produce is real and it is tens rather than hundreds.

**Both entry types arrive**, 61 % `X509LogEntry` against 39 % `PrecertLogEntry`, so the distinction is on
the wire and does not have to be inferred.
:::

### What the console reads, and what stays out of the registry

`discovery_source` does **not** join the [search registry](/architecture/search/#what-the-registry-holds).
It lives on `asset` and not on the projection, so putting it behind a filter means promoting a column for
a query nobody has asked for. The per program coverage panel reads the counters above and one grouped
query over `asset`, which is not a search.

That is the same call [10.3](/architecture/search/#what-the-registry-holds) makes about `title`: the day
somebody asks to filter an inventory on where a name came from, it is an `ALTER` and a line in that table,
in that order.

## 7.6 Future sources

Reverse WHOIS and ASN walking for organization discovery, archives such as Wayback for historical
URLs, public code repositories for secrets and endpoints.
