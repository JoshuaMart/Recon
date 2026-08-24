---
title: Asset lifecycle
description: Death is a property of a layer, not of an asset. The state machine, the scheduling, and the backoff curves.
sidebar:
  order: 6
---

## 6.1 Death is per layer, not global

A single `alive` boolean flattens signals that mean very different things:

| Layer | Death signal | Reliability | Reading |
|---|---|---|---|
| dns | `nxdomain` with resolver consensus | **strong**, the only clean one | the name no longer exists |
| dns | resolves, CNAME toward a dead service | strong | **takeover candidate**, a finding rather than a death |
| tcp | every port refused | strong | the host answers, nothing listens |
| tcp | every port timing out | **none** | indistinguishable from filtering or an IP ban |
| http | port open, recognized dead origin signature behind a CDN | **strong, conditional on the signature being known** | origin dead, edge alive |

Each layer therefore carries its own state, its own `last_ok_at`, its own `last_checked_at` and its
own failure counters, in [`asset_layer`](/architecture/data-model/#42-main-tables).

The reliability of the HTTP line is not intrinsically low. It depends on the coverage of the signature
set. A recognized signature is a strong signal; an unknown one produces no signal, not a weak one. The
practical consequence is to invest in signatures rather than in weighting.

**"Live DNS, dead origin" is the most interesting combination of the table** and deserves priority
alerting.

Two readings that look like death and are not:

- **`nodata` is not `nxdomain`.** A name that exists without an address, an MX only host, a TXT
  validation record, answers NOERROR with an empty section. Only `nxdomain` says a name does not
  exist. Confusing them would delete every mail host from an inventory.
- **A host a run never reached produces no observation at all.** FastRecon reports it as `discovered`
  rather than dead, and Recon writes nothing rather than inventing a verdict. Silence is not a
  measurement, and this is the same rule as [P2](/architecture/principles/) one level down.

An asset that no probe gets a result on belongs to none of these lines. It is not dead, it is
**unobservable**, and its silence must never be read as an absence of change.

## 6.2 State machine

```
                     probe OK
      ┌──────────────────────────────────────────┐
      │                                          │
      ▼                                          │
  [ACTIVE] ──probe KO──> [FLAPPING] ──N fails──> [INACTIVE]
      ▲                       │                       │
      │                       │ probe OK              │ M days
      └───────────────────────┘                       ▼
                                                 [ARCHIVED]
  [CANDIDATE] ──probe OK──> [ACTIVE]          (out of the scheduler,
       │                                        never deleted)
       └── budget spent ──> [ARCHIVED]

  [any state] ──no observer gets through──> [UNOBSERVABLE]
       ▲                                            │
       └────────── one probe gets through ──────────┘
```

- **CANDIDATE** was discovered but never verified alive. Typically from Certificate Transparency, or a
  port found open and not yet probed.
- **ACTIVE**, the last verification succeeded.
- **FLAPPING**, consecutive failures below the threshold. A buffer state against false positives.
- **INACTIVE**, death confirmed. Still watched, at a low rate.
- **UNOBSERVABLE**, neither the HTTP probe nor the fingerprinter gets a result. Neither alive nor dead:
  nothing can be said ([8.6](/architecture/verification/#86-reachability-per-observer)).
- **ARCHIVED**, out of the scheduler. Reactivated manually or on rediscovery.

An archived asset carries no due date, so nothing selects it and the only observation that can reach one
is an enumeration finding it again. That is what rediscovery means here, and it is why a **success**
brings it back while a failure does not: an observation that measured nothing must not pull an asset
into a queue it left. The reading is on the observation and never on the layer's state, for the same
reason [leaving `unobservable`](#64-qualifying-a-failure) reads the current observation: a layer keeps
the verdict of its last conclusive measurement, so an asset archived long after a success still carries
a healthy layer, and reading that column would let a timeout revive it. The other way back is somebody
entering the asset [by hand](/architecture/scope/#entering-an-asset-by-hand), which is an act rather
than an observation.

### Where the transitions live

Transitions are computed **in the ingestion path**, in the same transaction that writes the
observation. Three reasons:

1. **Consistency.** An asynchronous component can fail between the write and the transition, leaving an
   asset in a state that contradicts its last observation.
2. **It needs the shape of the observation.** [Qualifying a failure](#64-qualifying-a-failure) means
   telling an `nxdomain` with consensus from a refusal, a timeout and a recognized CDN signature. A
   separate component would re-parse what ingestion just parsed.
3. **Counters move together.** The two failure counters and `first_failure_at` are written at the same
   instant; separating them opens a window where they contradict the `lifecycle`.

Separation of concerns stays, at the level of the code rather than the component:

```
next_lifecycle(current_state, observation, counters) -> (state, counters)
```

A pure function, called by ingestion, testable on its own.

Out of the transaction: the **sending** of notifications, which can lag without consequence. Producing
the event is inside it, for the reasons above and especially the second
([12.3](/architecture/notifications/#123-where-the-event-is-produced)).

## 6.3 Scheduling and backoff

There is no queue table and no lease. Three due dates and a backoff tier live on
[`asset_current`](/architecture/data-model/#42-main-tables), and the scheduler selects what is due:

```sql
SELECT asset_id, key FROM asset_current
WHERE next_resolve_at <= now()
  AND lifecycle <> 'archived'
ORDER BY next_resolve_at, asset_id
LIMIT $batch;
```

The selected keys become the target list of one run
([9.1](/architecture/deployment/#91-the-run-contract)). Ingestion of the report reschedules them. A run
that dies takes nothing with it: the due dates were never moved, so the next tick selects the same
assets again.

**The tiebreaker is not decoration.** Assets are written in bulk, so thousands of rows routinely carry
the **same** due date to the microsecond: one report writes them in one transaction. With `LIMIT` over a
set of ties and nothing to break them, which rows come back is the planner's choice, and it can differ
between two ticks for reasons nothing in this document controls. Ordering on the identity as well makes
the walk deterministic, which is what lets a stalled batch be reasoned about at all.

**A clump of identical due dates is normal and needs no spreading.** What a due date decides is
*eligibility*; what goes out is decided by the batch size, by the concurrency of the pass and by the
program's budget. Spreading the clump would buy nothing and cost freshness, which is the same argument
that keeps a [baseline due when it is earned](/architecture/verification/#the-baseline-filter).

### Scheduling is per host

The target list is always a list of **hosts**, `fqdn` or `ip`, never services. A verification run scans
the curated port list on each host, exactly as discovery does, which is also what finds a port that
opened last week.

So `next_resolve_at` and `next_full_at` live on `fqdn` assets. A `service` is observed through its
host's run and carries only `next_fingerprint_at`, which is genuinely its own because rendering is per
service.

| Kind | Layers it carries | Comes from |
|---|---|---|
| `fqdn` | `dns`, `tcp` | its own run |
| `ip` | `tcp` | the hosts that resolve to it |
| `service` | `http` | its host's run |
| `url` | `http` | its service's run, and a render of its own |

An address is never a target of its own, and that is not a workaround: an address only ever enters this
inventory as the answer to a name, so the name is where the schedule belongs
([7.2](/architecture/discovery/#what-the-list-may-contain-and-why-recon-satisfies-it-by-construction)).

**A service carries no `tcp` layer of its own**, and the reason is the same one that makes it a
candidate when it is derived: the port sweep is an observation about the *host*, so it fills the host's
`tcp` layer and not the service's. What addresses the service is the HTTP probe, and a port carrying no
HTTP service is a port nothing has spoken to. Writing the sweep's result onto the service as well would
report every open port as a verified application, which is exactly the claim
[8.1](/architecture/verification/#an-open-port-becomes-an-asset) refuses to make.

### The stage ladder is the cost knob

FastRecon's scope is a ladder, and it replaces having one cadence per probe type:

| Due date | Stage scope | Cadence | Reason |
|---|---|---|---|
| `next_resolve_at` | `resolve` | 24 h | one round trip to the resolver pool, and nothing is sent to the target |
| `next_full_at` | `full` | 72 h | a hundred connections per host plus an HTTP probe, by far the most expensive |
| `next_fingerprint_at` | not a FastRecon run | 21 d | the default of [8.3](/architecture/verification/#83-when-a-render-happens), modulated by volatility |

An asset due for `full` does not need a `resolve` run: `full` runs every rung below it. These are
configuration values, not constants.

### Who fills the due dates

**Ingestion creates them**, in the transaction that creates the asset. A periodic sweep is rejected: it
would add a latency incompatible with the aggressive backoff of Certificate Transparency candidates. On
a first tier of one minute, a sweep every five minutes makes it inoperative. Ingestion also already
knows the `scope_status` in the same transaction, which a sweeper would have to reread.

| Source | First due date | Scope |
|---|---|---|
| Certificate Transparency | immediate, no jitter | `resolve`, then `full` once it answers |
| Discovery run | `now() + jitter(0 to 15 min)` | the run already reached it |
| Manual entry | immediate | **`full`** |

The jitter is necessary: without it, the thousands of assets of one discovery run share a due date and
come back together forever.

**A Certificate Transparency candidate takes no jitter on its first rung**, and it is the only source
that does not. The certificate is the event: something was published seconds ago and the whole of the
aggressive curve below rests on the first check happening now rather than inside a quarter of an hour.
What it costs is a clump, and a clump of due dates is
[already the normal case](#63-scheduling-and-backoff) rather than a thing to spread.

**A hand-entered host is due for `full`, not for `resolve`.** Somebody typed it in to find out what it
exposes, and a resolution would only report that the name answers. The ladder makes this free to say:
`full` runs every rung below it, so one run gives the resolution, the open ports and the services behind
them. That is also the only case where the first run of an asset is the expensive one, and it is the
right place for it, because a person is waiting.

**A hand-entered URL is a path somebody named**, which is the one case where a path is an identity
rather than the place a redirect landed. Adding one creates or finds the **service** it belongs to and
schedules that service, because a URL has no liveness of its own: what answers is the service. The URL
earns its render once its service has answered, through the ordinary
[baseline filter](/architecture/verification/#the-baseline-filter), and the renderer is given the URL as
declared rather than the service root. The distinction is the same one
[4.3](/architecture/data-model/#the-unit-of-a-web-asset-is-the-service-never-the-path) draws: a scanned
path is a byproduct, a declared path is an act.

**Only `in_scope` assets are scheduled.** `out_of_scope` and `unknown` are stored without due dates.
They are kept and displayed, never probed. A [scope reclassification](/architecture/scope/#52-re-evaluated-at-ingestion)
is the other write path: assets becoming `in_scope` get their dates, assets leaving lose them.

### Backoff curves

Two curves, and they answer different questions.

**CANDIDATE**, aggressive at the start:

```
1m → 5m → 15m → 1h → 6h → 24h → 3d → give up at about 14d
```

Between a certificate being issued and the service actually going live, anything from a few minutes to
a few days passes. Probing often during the first hour is what catches a service **as it appears**,
before it is hardened. That is where the freshness advantage actually is.

**FLAPPING**, patient, because the cost of a false positive is a useless alert:

```
15m → 1h → 6h → 24h → INACTIVE after 3 informative failures over ≥ 24 h
```

**Jitter applies to every delay**, including the nominal cadence. Without it, the assets of one
discovery run come back together on every round for good.

**Promotion to `full` is where a candidate takes its jitter**, and the reason is the one above rather
than a change of mind. A certificate carrying four hundred SANs promotes them within the same minute,
and `full` is not a rung, it is the entry into a **recurring** cadence: a convoy formed there comes back
every 72 hours for good, which is exactly what the jitter on a discovery run exists to prevent. The
immediate first rung forms no convoy, because a candidate that answers leaves the curve.

**A curve is written for the cheap rung, and the expensive one has a floor.** Fifteen minutes of
resolution is one round trip to a resolver pool. The same fifteen minutes of `full` is a hundred
connections per host plus an HTTP probe, four times an hour, and an asset reaches `FLAPPING` from the
`tcp` and `http` layers as readily as from `dns`, where the target is answering and the sweep costs
everything it says. Worse, a failing asset always holds the earliest due date, so it takes the head of
every batch and starves the inventory of its nominal passes. So the curve is clamped upwards on `full`,
at a value that is configuration. The floor is on a backoff and never on the schedule: an asset that is
fine keeps its nominal cadence.

`total_attempts` separates "given up after forty tries" from "never managed to test". The second
category usually points at a local problem, a resolver or a banned address, not at the target, and it
has to stay visible.

## 6.4 Qualifying a failure

The [reachability table](/architecture/verification/#86-reachability-per-observer) measures **whether
the observer reaches the target**. The lifecycle measures **whether the target is alive**. Feeding both
from one undifferentiated counter produces "dead asset" on targets that merely became unobservable.

> An asset moves to `INACTIVE` only when the failure is **informative**: at least one observer reached
> the target and reported an absence. A failure where no observer reaches the target leads to
> `unobservable`.

`unobservable` never passes through `FLAPPING`. It is not an unstable asset, it is an unmeasurable one,
and the transition starts from any state.

| Signal | Nature | Informative |
|---|---|---|
| `nxdomain`, resolver consensus | negative answer from the target | **yes, strong** |
| Connection refused | the host answers, nothing listens | **yes** |
| Dead origin signature behind a CDN | application level answer | **yes**, on the `http` layer |
| TCP timeout | no answer, indistinguishable from a ban | no |
| `servfail` | a resolution fault, not an existence fault | no |
| 403 or a WAF challenge | the target is alive | no |
| Fingerprinter failure | an observer fault | no |

**The CDN case is the one that forces the third line.** On a fronted target no informative transport
level failure ever happens: the edge always answers, with no refusal and no `nxdomain`. An asset whose
origin is dead would stay `ACTIVE` forever. Death there is readable only in the
[HTTP semantics](/architecture/verification/#dead-origin-behind-a-cdn).

### A degraded run cannot conclude a death

A scanner validates its resolver pool before a run, and when the whole pool fails validation the right
call for a scanner is to continue on the unvalidated pool and say so rather than refuse. A pool where
every resolver fails is far more often a local condition, no egress on port 53, a captive network, a
blocked anchor, than a genuinely hostile pool, and refusing would turn a network problem into no report
at all, which is the one outcome that cannot be inspected afterwards.

It is the wrong input for a death. A resolver that hijacks `nxdomain` turns every dead host into a live
one; one behind a captive portal turns every live host into a timeout. Those are precisely the two
signals this section qualifies.

**Rule: a report that says it ran degraded produces no informative failure.** The observations are
written, because they happened, but an `nxdomain` from a degraded run is qualified `error` rather than
`fail`, so the assets it names drift toward `unobservable` and never toward `INACTIVE`. That is the
correct reading rather than a precaution: a degraded observer is an observer, not a verdict.

The same holds for everything else a run says about itself. A truncated run already reports it, and a
wildcard sweep that hit its zone cap leaves some artifacts unclassified and says how many.

**This has to read a machine readable signal.** Matching the text of a warning works, and stops working
in silence the day the wording changes, which is the failure mode this document spends its length
avoiding.

### Where the qualification is carried

`observation.outcome` takes three values, and they are not "succeeded / failed / crashed". They are the
table above:

| `outcome` | Meaning | Effect on the lifecycle |
|---|---|---|
| `ok` | the target answered, and it is there | success, moves `FLAPPING` back to `ACTIVE` |
| `fail` | the target answered, and it is not there | **informative failure**, the only value leading to `INACTIVE` |
| `error` | nothing conclusive was obtained | non informative, feeds `unobservable`, never `INACTIVE` |

Two values would have forced re-parsing the payload to recover the information. The third carries it
where the counter is incremented.

**`outcome` says nothing about reachability.** It answers "is the target there?", which is orthogonal to
"did my probe get anything usable?". Probes produce a second value, `usable`, for the second question
([8.6](/architecture/verification/#outcome-and-usable-are-orthogonal)).

**The control plane recomputes this value, it does not believe it.** Scanners are untrusted by
assumption ([P6](/architecture/principles/)), and a scanner declaring `fail` on a timeout would archive
live assets. The qualification is derived at ingestion from `(layer, data)`, in the transaction that
writes the observation, exactly where transitions already live.

### `INACTIVE` wins, and that is the rule rather than an exception to it

A dead origin behind a live edge is an informative failure: the probe reached the edge, and the edge
reported that the origin is gone. It is `usable = false` only because the probe learned nothing about
the *service*, which is a different question. A death that was observed is not a silence, and such an
asset must end `INACTIVE` even if the fingerprinter fails at the same moment.

**Leaving `unobservable` reads the current observation, not the column.** The threshold of three
concordant results applies in both directions, so the `reachable` flag would take three passes to turn
over. Reading the flag would hold an asset in that state two rounds after it started answering again,
while the table asks for a single success.

**Both observers must have tried.** A streak at zero is an observer that never ran, not one that fails.

## 6.5 Thresholds

| Transition | Condition |
|---|---|
| `ACTIVE → FLAPPING` | 1 informative failure |
| `FLAPPING → INACTIVE` | 3 consecutive informative failures, over **≥ 24 h** |
| `FLAPPING → ACTIVE` | 1 success |
| `* → unobservable` | 3 consecutive non informative failures, no observer getting through |
| `unobservable → *` | 1 success from either observer |

Three failures are enough because they are **qualified**: an `nxdomain` confirmed three times with
resolver consensus leaves no ambiguity. A larger count would be compensating for the absence of
qualification with volume.

The 24 hour floor is still necessary. With the backoff of [6.3](#63-scheduling-and-backoff), three
failures can happen in ninety minutes, which is not long enough to tell an outage from a disappearance.
`first_failure_at` is what makes the condition checkable at all.

The fingerprint layer fills its counters but **triggers no lifecycle transition**. A render failure
feeds `fingerprint_streak`, never `FLAPPING` or `INACTIVE`, which is what the last line of the
qualification table already says.

### Three layers, one `lifecycle`

**The most severe layer wins.** Each layer first produces its own reading from its counters:

| Layer reading | Condition |
|---|---|
| never measured | no attempt |
| healthy | no failure in progress |
| failing | at least one failure, threshold not reached |
| dead | informative threshold reached, 24 h floor included |

The asset then takes the state of the worst layer that has an opinion: a dead layer gives `INACTIVE`, a
failing layer gives `FLAPPING`, otherwise `ACTIVE`. Layers never measured are ignored.

The case that forces this rule is the CDN one. On a fronted asset whose origin is dead, `dns` resolves
and `tcp` connects, because the edge answers for both, while `http` returns a recognized dead origin
signature. Under any rule where a success somewhere restores the asset, it stays `ACTIVE` forever and
the only death signal available behind a CDN is thrown away.

The counterpart holds: a success on the failing layer resets its counters, so the asset goes back to
`ACTIVE` **in a single probe**, which is what the threshold demands.

## 6.6 An asset that was never alive

A Certificate Transparency candidate whose infrastructure was never provisioned goes straight to
`ARCHIVED` when its backoff budget runs out, without passing through `INACTIVE`. It is not dead. It
never existed.
