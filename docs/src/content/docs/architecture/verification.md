---
title: Verification
description: Probing the known inventory, deriving services from open ports, and the browser that renders what a raw client cannot.
sidebar:
  order: 8
---

Verification asks one question of the inventory: what still answers. It is a
[FastRecon run over an explicit target list](/architecture/discovery/#two-inputs-two-mandates), which
is the only shape in which a missing answer means something ([P2](/architecture/principles/)).

Alongside it runs the **Fingerprinter**, a browser based service. The split between the two is not a
matter of taste. FastRecon is a **detector**: is it alive, has it moved, as cheaply as possible. The
Fingerprinter is an **enricher**: expensive, never running at the cadence of a probe. And when the raw
client is blocked, the enricher becomes the detector ([8.6](#86-reachability-per-observer)).

## 8.1 What a verification report concludes

### An open port becomes an asset

A port found open on a host creates the corresponding `service` asset, in the transaction that writes
the observation. Without that rule the port scan buys nothing: no asset means no due date, no HTTP
probe on that port, no service detected, and nothing reaching the fingerprinter. A forgotten Jenkins on
8090 is the reason to scan, and the scan that finds it would have no way to put it in the inventory.

**Ingestion derives it, not the scanner.** The control plane rereads the payload and concludes for
itself, the same way it recomputes `outcome`. Two things that forces, otherwise the door
[P6](/architecture/principles/) closes reopens from the side:

- **The host of the derived key is the host of the observed asset**, never a `host` field from the
  payload. A scanner given target A cannot manufacture services on B.
- **The port numbers come from the payload.** What a lying scanner can therefore do is declare ports
  open on a host it already holds, and the bound below plus
  [scope re-evaluation](/architecture/scope/#52-re-evaluated-at-ingestion) make that pointless.

**The bound.** Beyond 24 open ports on one host, nothing is derived and the fact is logged. A host
answering on a quarter of the curated list does not have twenty-five services: it is a tarpit, a device
accepting every connection, or an edge answering for everything behind it. The observation keeps its
full port list either way, so the finding stays readable even when it does not become an asset.

The derived asset is `CANDIDATE` with `last_checked_at` NULL. The port scan is an observation about the
*host*; the service itself has not been probed yet. Its lineage is real: `parent_asset_id` is the host,
and `discovery_path` records which run found which port. Only a host derives services. A `service`
asset makes its own port scanned and nothing else, so deriving from it would recreate itself.

### What the port sweep can conclude

A report that lists only open ports cannot conclude a death, and the reason is the same
[P2](/architecture/principles/) one level down: an empty list reads as "nothing was open" and as "nothing
was tried" at once, and those are opposite findings. A host behind a firewall that started dropping is
indistinguishable from a host that closed everything.

So the report carries what the sweep actually tried, as counts per host:

```json
{ "scan": { "scanned": 100, "open": 0, "refused": 100, "filtered": 0, "unknown": 0 } }
```

| Counts | Reading | `outcome` |
|---|---|---|
| `open > 0` | something listens | `ok` |
| `open = 0`, `refused > 0`, `filtered = 0`, `unknown = 0` | the host answers, nothing listens | **`fail`**, informative |
| `open = 0`, any `filtered` or `unknown` | indistinguishable from a ban, or never measured | `error` |
| the buckets do not sum to `scanned` | the counts cannot be read | **no observation** |
| no counts at all | the sweep said nothing | **no observation** |

**A single filtered port breaks the death**, and that is deliberate rather than cautious: what is
filtered is exactly what could be hiding a service, and a verdict that ignores it would archive hosts on
the strength of the ports that were *not* interesting.

**The four buckets have to add up**, and that is the same rule one level down. A probe that failed on a
local limit says nothing about the target and belongs in neither `refused` nor `filtered`, so it gets a
bucket of its own; a sweep that dropped it silently would leave "every port refused" true over a set of
ports that was never tried. Counts that do not cover what the sweep says it attempted are counts this
cannot read, and reading them anyway concludes a death over the difference.

Until the scanner carries the counts the last line applies, so the `tcp` layer writes nothing rather than
a verdict it cannot support. Nothing else degrades: `dns` and `http` conclude on their own evidence, and
the layer that cannot speak stays silent.

### Dead origin behind a CDN

Roughly a quarter of a real perimeter sits behind a CDN or a cloud edge, and on such an asset three
things stop being true at once: the port scan lies, because the edge opens the web ports for everyone;
HTTP liveness lies, because the edge always answers; and geolocation lies, because the address is the
point of presence.

So the presence of a response is not liveness. The **semantics** of the response has to be read:

| Signature | Meaning |
|---|---|
| Cloudflare error 1016 | origin DNS error, takeover candidate |
| Cloudflare error 1001 | DNS resolution error |
| 404 with `Server: AkamaiGHost` and a reference number | origin dead, edge alive |
| 403 with a mitigation signature | alive but protected, not dead |

The first three count as **informative failures** on the `http` layer
([6.4](/architecture/lifecycle/#64-qualifying-a-failure)). On a fronted target they are the only death
signal available, since transport stays healthy permanently. They are derived at ingestion from the
status code, the `server` header and the title, and from the rendered body once the fingerprint layer
has run.

**A 403 is not a 403.** The last row only holds for a 403 carrying a mitigation signature. An
application answering 403 on a protected route is alive and measured: that is a **success**. A
challenge is a **non informative failure**: the target is alive, so it must never drift toward
`INACTIVE`, but the raw client measured nothing, so it must not count as a success either. The browser
may get through, and that is exactly the `✗ / ✓` regime of [8.6](#86-reachability-per-observer).

Without the distinction one of two errors is certain. Either every 403 becomes a success and the regime
switch never happens, or every 403 becomes a failure and every protected route in an inventory drifts
to `unobservable`.

### Takeover candidates

Two halves, one field.

A **dangling CNAME** arrives directly in the report: a host with `status: dead`, `reason: nxdomain`, and
a non empty `cname`. A recursive resolver follows the chain itself and returns `nxdomain` with the
CNAME still in the answer section, so the name pointing at nothing and the proof arrive together, in one
query.

The other half, a CNAME toward a cloud service that still resolves, is invisible in DNS: the name
exists. It reads in the service's own response, which says nobody has claimed the bucket or the
application.

Both write the same structured field in `observation.data`:

```json
{ "takeover_candidate": { "kind": "orphan_cname",     "target": "bucket.s3.example.net", "signature": "nxdomain" } }
{ "takeover_candidate": { "kind": "unclaimed_service", "target": "https://www.acme.test/", "signature": "s3" } }
```

The [Notifier](/architecture/notifications/) reads this field to produce the event. A bare counter would
have forced rewriting the probe the day the alert needs to carry *what* is vulnerable and *on what
evidence*.

**The timestamp is added at ingestion, not by the probe**, and this is not plumbing. A date inside
`observation.data` would differ on every pass, so a dangling CNAME probed hourly would write a row an
hour and defeat [deduplication](/architecture/data-model/#deduplication-on-write) on exactly the assets
worth following. The probe emits what is stable, `kind`, `target`, `signature`, and the control plane
copies the finding into `asset_current.attributes` adding `detected_at`, a column nothing deduplicates
on.

What the notifier then gets for free is the **full window**: `observed_at` and `last_confirmed_at` are
the two ends of the period during which the finding was true. One row, held open.

## 8.2 The Fingerprinter

A separate long-running HTTP service in its own repository, written rather than adopted. Go, a pool of
headless Chrome instances joined over CDP, one browser context per scan.

```
POST /scan
{ "url": "https://example.com", "options": { "screenshot": false } }
```

Options: `timeout_seconds`, `max_redirects`, `skip_path_checks`, `screenshot`.

| Response field | Contents |
|---|---|
| `chain[]` | one entry per hop: `url`, `status_code`, `headers`, `title`, `response_size`, `remote_ip_address`. **Ordered**, the sequence is the information |
| `technologies[]` | `name`, `version`, `category`, `cpe`, and a structured `proof` naming the evidence the detection rests on |
| `cookies` | name to value |
| `scripts[]` | `url`, `internal`, `hash`, the SHA-256 of the content, **per script** |
| `metadata` | `robots_txt`, `llms_txt`, `sitemap`, and the three favicon fields below |
| `external_hosts[]`, `web_sockets[]` | third party hosts contacted during the render |
| `network` | `host`, `ips`, `cname`, the service's own resolution of the target |
| `screenshot` | base64, only when asked for |
| `version`, `scanned_at` | the build that produced the result, and when |

This is what FastRecon does not give: full headers, cookies, per script hashes, a favicon, and detection
with evidence. The pipeline is browser first, so the redirect chain is what Chrome actually followed,
including what JavaScript did.

### The favicon, and why three fields

| Field | Contents |
|---|---|
| `favicon` | the **bytes**, as a `data:` URI, however the page carried them. Null past the inline size bound |
| `favicon_url` | the source, when the icon was linked. Null when it was inline |
| `favicon_hash` | mmh3 over the RFC 2045 base64 of the bytes, Shodan and Fofa compatible. Present whenever there is an icon |

**The hash has one entry point.** Both paths end on the same bytes and the hash is computed there. The
trap is concrete rather than theoretical: one branch hashing raw bytes and the other their base64 gives
two values for one icon, [`pivot_count`](/architecture/search/#105-pivots) splits, and nothing raises an
error.

**Two absences that do not mean the same thing.** A null `favicon_url` says the icon was inline; a null
`favicon` says it was over the size bound. A consumer confusing them would conclude that a 90 KB icon
does not exist, when it has a hash, a counter and a badge, and only the thumbnail is missing.

### Saturation, and why nothing is lost

The service answers **429 when its pool is saturated**, with a `Retry-After` in seconds. The refusal comes
when no slot frees up within its acquire timeout, not the instant the pool fills: a scan lasts a few
seconds, so refusing on the spot would reject callers a slot was about to open for.

**A 429 is a state of the service, so it must not touch the asset.** No observation, no counter, no
streak, no `last_fingerprint_at`. This is the rule of
[a probe error against a measurement failure](#a-probe-error-and-a-measurement-failure-are-two-things),
and it is the trap worth naming: a render that reached the target and got nothing writes an observation,
while a render that never happened writes nothing at all. Confusing the two walks assets toward
`unobservable` for a reason that has nothing to do with them, and `unobservable` is supposed to mean the
target cannot be observed, not that we were busy.

**Nothing is lost, and the reason is structural rather than a retry.** A render has no lease: the due
date **is** the queue. A refused asset simply keeps `next_fingerprint_at` in the past, so it is still due
on the next tick. There is no state to reconcile, no reservation to expire, and no path where a crash
between the refusal and the retry drops the work. The only way to lose a render would be to move the due
date on the way out, which is exactly why it moves on ingestion and nowhere else.

**Refused work keeps its place, without a priority mechanism.** Selection orders on the due date, and a
refused asset's date is older than everything queued behind it. So it comes back first on the next pass by
the same ordering that put it there, and the two [priority queues](#two-priorities) keep meaning what they
mean.

**Waiting is bounded, and jittered.** The caller honours `Retry-After` and spreads it between half and one
and a half times the announced delay: everyone refused at the same instant received the same value, and
waiting exactly that long reconstitutes the convoy the refusal was meant to break. After a few consecutive
refusals the pass stops rather than keeps knocking, because a saturated service will refuse the next four
hundred targets for the same reason. Stopping costs nothing here, which is the point of the paragraph
above.

**A refusal gives the budget back.** The [render budget](/architecture/deployment/#95-rate-limiting) is
charged before the call, because charging after would let a burst reach the target before anything counted
it. A 429 means nothing reached the target, so the charge is returned. Without that, a saturated service
would throttle a program's real renders on behalf of renders that never happened, which is the opposite of
what the budget protects.

**Saturation is visible without reading logs.** It shows as the fingerprint queue deepening, which is the
one [queue depth](/architecture/deployment/#99-reading-the-queue) worth watching: its unit costs two orders
of magnitude more than a probe, so its depth translates directly into minutes of browser.

## 8.3 When a render happens

Five triggers:

1. **First contact**, whatever the HTTP status code, including 403, 401, a challenge or an origin error
   behind a CDN. The browser can get a result the raw client cannot.

   The target is the service, at its root, because a path a scan landed on describes a redirect rather
   than a surface. The exception is a **declared** URL, which is rendered as it was written: somebody
   named that path, which is the whole difference between an identity and a byproduct
   ([6.3](/architecture/lifecycle/#who-fills-the-due-dates)).
2. **A change detected by the HTTP layer**, in the nominal regime only.
3. **Periodic refresh**, 21 days by default, modulated by volatility. 7 days when the fingerprinter is
   the only detector.
4. **A manual request** from the console.
5. **A major update of the service** ([8.7](#87-dating-the-instrument)).

The last two are API entry points. They require `manage_jobs` rather than `ingest`, and the distinction
matters: something holding `ingest` could otherwise schedule renders of its choosing and spend a
program's budget on targets it picked.

### The baseline filter

Trigger 1 is the one that needs a filter, because a discovery run produces thousands of `CANDIDATE`
assets at once and a browser baseline on all of them saturates the service while the genuinely urgent
renders wait. The filter reads **transport reachability and nothing else**:

| Result | Baseline |
|---|---|
| `nxdomain` with resolver consensus | no |
| Connection refused on every port | no |
| Port on Chromium's blocked list | no |
| Timeout | **yes** |
| 403, challenge, WAF page | **yes** |
| Origin error behind a CDN | **yes** |
| Normal response | yes |

**It is not a filter on `outcome`**, and that is the mistake to avoid while implementing it. An origin
error behind a CDN is an informative failure, counted as proof of death above, and it still deserves a
baseline, because an edge answering for a dead origin is a page with something to read. Deriving this
filter from the qualification would get that case exactly backwards.

The blocked port line is a fact about the instrument, not a hypothesis about the target: Chrome answers
`ERR_UNSAFE_PORT` on some ports, so the failure is certain before the call. A certain failure is not a
measurement, and it would push an SSH service toward `unobservable`, a state that qualifies what is
unknown rather than what is not the web. The list is **Chromium's own**, not a hand written list of
"non web" ports, which would be the hypothesis.

**The consequence is assumed**: on these assets `fingerprint_reachable` stays undefined. That is
correct. `unobservable` qualifies assets whose state is unknown, not ones whose target explicitly
signalled absence, and there is no uncertainty about what a browser sees on a port it refuses to open.

The filter applies to trigger 1 only. An already baselined asset that becomes `nxdomain` belongs to the
[lifecycle](/architecture/lifecycle/#64-qualifying-a-failure) and is not resubmitted.

**A baseline is due when it is earned.** It does not inherit the discovery jitter: that spread exists for
the first probe of a freshly discovered asset, and the render line is created later, when an observation
has proved the target worth rendering. The herd has already been spread once, and spreading it twice
adds no smoothing.

### The render pass

The queue is a **predicate, not a list**:

```sql
SELECT asset_id, key FROM asset_current
WHERE next_fingerprint_at <= now()
  AND lifecycle <> 'archived'
ORDER BY fingerprint_priority, next_fingerprint_at, asset_id
LIMIT $batch;
```

It is re-evaluated on every tick and holds no state between two of them. That is what makes the whole path
idempotent, and it is the reason the section below can claim nothing is lost without pointing at a
recovery mechanism.

**Priority sorts before the due date**, and that ordering is what protects the urgent case. A first scan
makes thousands of baselines due at the same instant, and a render triggered by a detected change five
minutes later carries a *later* due date. Ordered on the date alone it would sort behind every one of
them. In the `high` queue it goes first, which is the whole point of
[having two](#two-priorities).

The service takes **one URL per call**, so a pass is a bounded number of calls made concurrently, never a
list handed over.

**The budget is the governor, not the concurrency.** With a program at ten requests a second and a render
charged thirty, that program can afford one render every three seconds; a render takes a handful of
seconds, so four or five in flight is where the budget starts binding. The concurrency is set above that
figure rather than at it, so that the thing throttling a program is its published rate limit and not a
number nobody calibrated.

That arithmetic is also what bounds a flood. Ten thousand services becoming due at once, which is what a
first scan of a large perimeter produces, is about eight hours of budget for that program alone. The
service is not what a single program saturates.

A pass ends in one of three ways, and none of them writes anything: the selection comes back empty, the
program's budget refuses to wait, or the service refuses often enough to say it is busy.

### Two priorities

| Queue | Contents | Handling |
|---|---|---|
| `high` | change detected, manual request | immediate |
| `low` | new asset baseline, periodic refresh, refresh after a service update | spread out |

A change on a followed asset must never wait behind a mass discovery.

### The economics

A browser render costs 2 to 10 seconds and several hundred megabytes, against milliseconds for an HTTP
probe. On 50 000 services verified daily, this split produces 50 000 probes and a few hundred renders.
Routing all verification through the browser would multiply infrastructure cost by an order of magnitude
for nearly identical information.

## 8.4 Why the service is written rather than adopted

Three reasons, by decreasing weight.

1. **The contract above is specific.** Per script hashes with an internal flag, `external_hosts`,
   `metadata`, an mmh3 favicon hash: no existing tool produces this shape. Wrapping one would mean
   writing the adapter **and** inheriting the upstream's decisions.
2. **[8.7](#87-dating-the-instrument) requires control of versioning.** Classifying a diff as a
   revelation or a real change assumes knowing what changed in the instrument between two versions. On a
   third party image, `producer_version` is an opaque tag and the classification degenerates into blind
   heuristics.
3. **Detection signatures are an asset.** CDN and WAF classification, dead origin signatures,
   application patterns: knowledge accumulated across targets, and a real part of the product's value.
   Delegating it caps the platform at what someone else's tool detects.

Detections are YAML files loaded without recompiling, with Go detectors for the complex cases.

## 8.5 Network isolation

The service executes JavaScript controlled by the target. It must never sit next to the control plane:

- an isolated network, no route to PostgreSQL and none to the internal API;
- egress to the internet only, with explicit blocking of RFC1918, 127/8 and link local 169.254/16, which
  is where cloud instance credentials live;
- an outbound address distinct from the control plane's;
- called by the control plane, never the reverse.

Without these, a headless browser rendering arbitrary pages is an SSRF engine adjacent to the database.

**Isolation is a property of the application before it is one of the network.** These controls are
implemented **in the service**, with the network as the second line. They stay true whatever surrounds
the service, and they can therefore be exercised locally. The service refuses resolution toward an
internal range **before sending the request** and **at every redirect hop**: a network filter alone does
not cover a redirect to a name that resolves internally, which is the actual exploitation path, since it
never goes through the initial name's resolution.

Locally, two Docker networks. `control` carries PostgreSQL and the control plane; `scan` carries the
fingerprinter and its Chrome instances, and nothing else. Verification happens twice because the two
prove different things: one script queries the running stack and asserts that `psql` and `curl` toward
the internal API both fail from `scan`, and one test reads the compose file and fails if a shared network
appears. The first proves isolation works today, the second that a line added later did not undo it.

**Each refusal comes with its positive control**, and that is the part that counts. A refusal on its own
passes just as well against a stopped database, a wrong hostname or a typo in a network name, which is
the usual way an isolation test turns green without proving anything. So the script also checks that the
same connection **succeeds** from `control`, and reports *skipped* rather than *ok* when the target
component is not running.

## 8.6 Reachability per observer

Whether a target is measurable is a **relation between an observer and a target**, not a property of the
target, and neither observer is privileged:

- A browser that clears a challenge sees a 200 and never knows a WAF was there.
- A raw client taking a 403 detects it perfectly.
- The reverse exists too: a mitigation targeting a headless browser fingerprint, or requiring an
  interaction automation does not produce, lets a minimal HTTP client straight through.

So the strategy is driven by what is **measured**, not by what is deduced:

```sql
-- asset_current
http_reachable         boolean   -- a usable response was obtained
fingerprint_reachable  boolean   -- a usable render was obtained
```

| HTTP | Fingerprint | Reading | Detector | Cadence |
|---|---|---|---|---|
| ✓ | ✓ | nominal | HTTP layer | 21 d, modulated by volatility |
| ✗ | ✓ | mitigation aimed at the raw client, the common case | fingerprinter | 7 d fixed |
| ✓ | ✗ | mitigation aimed at the browser, or a failing service | HTTP layer | 30 d, with recovery attempts |
| ✗ | ✗ | **unobservable** | none | 7 d, alert |

Transitions need **three consecutive concordant results**, in both directions, to absorb transient
failures. They rest on two **signed** counters on `asset_current`, positive for consecutive successes and
negative for failures, so the threshold reads `abs(streak) >= 3` with one column per observer.

When the detector is not the HTTP layer, trigger 2 of [8.3](#83-when-a-render-happens) is disabled: the
probe keeps running for reachability and TLS, but its diff no longer triggers a render.

### `outcome` and `usable` are orthogonal

`outcome` qualifies **the target** and drives the
[lifecycle](/architecture/lifecycle/#where-the-qualification-is-carried). It is not enough to fill in
reachability, which qualifies **the observer**.

A 403 carrying a mitigation signature is `outcome = ok`, the target answered and it is there, and yet the
probe got nothing usable out of it. That is precisely the case that must flip the regime.

```
outcome   ok | fail | error   → lifecycle
usable    bool                → http_streak / fingerprint_streak
```

`usable = false` on a detected challenge, a 403 with a mitigation signature, or a recognized origin error
behind a CDN. `usable = true` on an application response, including a clean 401 or 404. Both values are
derived at ingestion, for the reason [P6](/architecture/principles/) gives.

Without this distinction the regime switch never fires on the most frequent case, a WAF answering a clean
403 rather than letting the connection expire.

**What proves a challenge is not the same on both sides**, and confusing them is expensive. On the HTTP
side, `waf_detected` is derived per response, from the status with the headers and the body, so it
already means "this response is a mitigation". On the fingerprinter side, a technology in the `WAF`
category means "there is a WAF here": Cloudflare is reported on every response it fronts, including a
normal 200. Using that as proof of a challenge would mark every legitimate 403 of a fronted application
as unmeasurable, roughly a tenth of a real perimeter, and push those assets toward `unobservable`. A
render is therefore judged on the **challenge page itself**, recognized by the body signatures above. A
challenge page is the same page whichever client fetched it.

### A probe error and a measurement failure are two things

The rule: **an error return is for a target the probe cannot address; a target that refuses, times out or
answers a challenge all come back as observations**, because each is a different answer and
[6.1](/architecture/lifecycle/#61-death-is-per-layer-not-global) treats them differently.

This applies to the fingerprinter too, and it is easy to get wrong there. When no candidate produces a
chain, a browser receiving `ERR_INVALID_HTTP_RESPONSE` on a BitTorrent port for instance, the service
addressed the target perfectly well: the target answered something that is not HTTP. That is
`outcome = error`, `usable = false`, **with an observation**. Returning a bare error instead leaves the
asset's counters untouched, so its backoff never widens, `fingerprint_streak` never moves, and the
`unobservable` verdict this very section provides for becomes unreachable.

What genuinely stays a probe error: a saturated service, an encoding failure, and a target for which no
candidate URL exists.

:::caution[`last_fingerprint_at` follows the render, not the observation]
Putting the render timestamp on **every** `fingerprint` layer observation would make a failure move it,
and the list would show "rendered five minutes ago, no cookies" on an asset no browser ever rendered.
That is exactly the false statement the [three cookie states](/architecture/search/#a-missing-cookie-badge-has-three-causes)
exist to prevent.

The timestamp moves when the payload carries a final hop, which is when a browser obtained a page. The
same test as the qualification, not a second criterion to keep in step.
:::

### `is_cdn` and `waf_detected`

`waf_detected` is a **descriptive attribute**, not a strategy switch. It records that a mitigation
signature was seen, with its source:

```json
{ "waf_detected": true, "waf_source": "http", "waf_vendor": "cloudflare" }
```

It serves display, filtering and pivots. Strategy is driven exclusively by the two reachability columns.
`is_cdn` is unchanged: structural, observed identically by both observers, unambiguous.

Signatures used at ingestion, and what a raw report can actually carry them in:

- **`is_cdn`**, the ASN and organization of the resolved address, the terminal CNAME (`*.edgekey.net`,
  `*.cloudfront.net`, `*.fastly.net`), and signature headers (`CF-RAY`, `X-Amz-Cf-Id`,
  `Server: AkamaiGHost`). FastRecon also determines CDN membership per address before scanning, and the
  report carries the provider name with a `scan_limited` marker.
- **`waf_detected`**, mitigation headers and cookies (`cf_clearance`, `incap_ses_*`, `X-Sucuri-ID`), and
  recognizable challenge bodies.

**The HTTP probe hands back a status, a server header, a title and a technology list, and no other
header.** So the signatures the raw layer applies are the ones expressible in those four, which covers
the origin errors and the interstitials, and not the header-only ones. The rest arrive with the render,
which sees the body and every header, and that is the ordinary division between the detector and the
enricher rather than a gap: a signature nobody can express writes nothing, and an unknown signature
produces no signal rather than a weak one.

Both are re-evaluated on every pass, never frozen. A target can move behind a WAF between two runs.

### Cadence of the periodic render

The HTTP layer does not detect application changes: a version bump modifies neither the kept headers nor
the certificate. A background render is therefore necessary independently of any detection.

A fixed interval wastes effort on frozen assets and undersamples active ones, so the period is derived
from data already in the database:

| Situation | Interval |
|---|---|
| Default | 21 d, configurable per program |
| A diff found at the last render | 7 d |
| No diff over 3 consecutive renders | 45 d, doubling up to a 90 d ceiling |
| `first_seen` under 30 d | 7 d |

Recent assets are being deployed, so they are the most unstable and the most interesting. An asset stable
across three passes does not need the same attention.

Two details, because the table is ambiguous on both. **A young asset wins over the stretch**: the
`first_seen` row is evaluated before the three renders row, since a three week old asset that has not
moved in three renders is far more likely to move than one silent for a year. And "no diff over 3
consecutive renders" counts renders that **found nothing new**, which a first render never is, so three
of them means the fourth render rather than the third.

The choice between the two candidate intervals happens **in the write**. Whether a render produced a diff
is what deduplication decides, inside the transaction that writes the observation, so both candidates are
computed in Go where the arithmetic is testable and the statement picks one. Rereading the previous
payload to decide would cost a round trip on every render.

The modulation applies to the nominal regime only. In the other three, the cadence of the reachability
table wins: stretching it would mean ceasing to observe exactly where the render is the only source of
detection.

## 8.7 Dating the instrument

Detection improves. An asset measured under one version as `[nginx]` and later as
`[nginx, Grafana, Prometheus]` has not changed. **The observer changed.** Untreated, the
[structured diff](/architecture/notifications/#121-structured-diffs) reads that as an application change
and alerts, potentially across the whole inventory after one update.

An observation is not a property of the world. It is a **measurement made by a dated instrument**.

Two columns, because one row outlives several versions: deduplication does not compare
`producer_version`, so an observation confirmed twenty times keeps the version of its first pass, and
`last_producer_version` moves with `last_confirmed_at`. The comparison is therefore between the previous
observation's `last_producer_version` and the new one's `producer_version`, which is the question being
asked: did the instrument change **between these two measurements**?

| Diff shape | Reading | Notification |
|---|---|---|
| Pure addition, `[nginx]` to `[nginx, Grafana]` | revelation, the instrument sees better | "detection improved", no urgency |
| Replacement, `nginx 1.24` to `1.25` | real change | normal |
| Pure removal | detection regression or real change | worth a look, low priority |

The heuristic is assumed: a real deployment happening on the day of a service update would be
misclassified. The trade converts a large volume of noise into a small number of ambiguous cases.

**Forced refresh after a major update**: the whole inventory is replanned in the `low` queue, spread over
several days, with the classification above active. That restores baseline consistency without a mass
alert.

The sweep places **its own work** in the low queue; it does not demote what is already waiting. A manual
request or a detected change lives in the high queue with an immediate due date, and a replan triggered a
second later would bury it for a week without trace. So the sweep may only **bring a render forward**,
never delay one, and it leaves untouched the assets that have left the scheduler.

## 8.8 Geo-IP and ASN enrichment

Enrichment happens **at ingestion, in the control plane**, never in a scanner. The MaxMind GeoLite2
database is around 70 MB, and shipping it to every run would impose that volume on every cold start. The
report carries the raw address; the control plane loads the `.mmdb` once in memory and refreshes it
weekly.

Derived fields: `asn`, `asn_org`, `country`, `region`, `city`.

**The address comes from the connection, not from DNS.** A port scan reports the address it actually
connected to, which is the only one it observed. That value is **declared and removed at normalization**,
like a duration: a round robin name answers a different address on every pass, and a payload carrying it
would differ on every run. It exists only to fill `asset_current.ip` and the derived columns, so
enrichment reads the **submitted** payload rather than the normalized one. That is the single exception
of its kind in the write path, and it is acceptable for the reason [P6](/architecture/principles/) already
gives: a scanner lying about this address lies just as well about its A records, and the control plane
parses the value before doing anything with it.

**An address only when there was a connection.** A refused port proves a host answered without any socket
opening, so there is no address to read; a filtered port proves nothing. A fully silent target stays
without geolocation, which is exact.

**Display restriction.** Geolocation is shown only when `is_cdn = false`. On a fronted target the address
is the point of presence and the location is misleading: a North American service can appear on another
continent depending on the PoP queried. The value stays stored, and the interface shows "CDN" instead.

**`asn` and `asn_org` are far more actionable than the country** and stay displayed in every case.

**The country's main use is anomaly detection.** On a program whose infrastructure is concentrated in one
region, an asset appearing in an unusual area is a signal: shadow IT, an undeclared contractor, inherited
infrastructure. The rule only means something if the program has a normal to compare against, so three
thresholds, all required:

| Threshold | Value | Reason |
|---|---|---|
| Volume | at least 50 geolocated non CDN assets in the program | below that the distribution is not significant |
| Concentration | the top two countries cover over 80 % of assets | a program spread over twelve countries has no normal |
| Alert | a country representing under 5 % of the program | that is the deviation itself |

Plus a **30 day learning period** after the program is created, during which the distribution is recorded
without alerting. Without it, the first assets define the normal and the fiftieth triggers an alert for
the sole reason that it arrived later.
