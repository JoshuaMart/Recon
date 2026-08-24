---
title: Notifications and diff
description: Structured diffs rather than opaque hashes, the events worth sending, and where each one is produced.
sidebar:
  order: 12
---

This is what turns an inventory into a product.

## 12.1 Structured diffs

A hash answers "did it change?". It does not answer "what changed?". Since the values are already in the
database, the hash is **worth less than the value it stands for**: `nginx 1.24.0 → 1.25.3` is actionable,
`a3f9... → 7b21...` is not.

Change detection therefore compares **normalized structures**, not hashes:

```
technologies : [nginx 1.24.0, PHP 8.2] → [nginx 1.25.3, PHP 8.3]
scripts      : app.min.js 2953e9... → a4f3e2...
chain        : 1 hop → 2 hops (302 to /login)
cookies      : + PHPSESSID
certificate  : not_after 2026-11-02 → 2027-01-14
```

Directly usable in a notification and in the interface, which an opaque hash is not.

**The constraint is normalization.** Comparison runs on normalized structures: sorted lists, parsed
versions, uniform case. Without it, a reordering in a response produces a false change, which is exactly
the fault hashes were supposed to avoid. It is the **same** normalization as at ingestion, produced by the
same function ([4.5](/architecture/data-model/#normalization-comes-first)). Two divergent implementations
would reintroduce the false change they exist to remove.

The surviving hashes serve one purpose only: being an indexable equality key shared between assets, which
is a [pivot](/architecture/search/#105-pivots).

## 12.2 Events worth notifying

| Priority | Event | `kind` |
|---|---|---|
| Critical | Takeover candidate (dangling CNAME, unclaimed bucket, origin error behind a CDN) | `takeover_candidate` |
| Critical | External host dead while a script still points at it | `external_host_dead` |
| High | New `ACTIVE` asset, especially from a Certificate Transparency candidate | `new_active` |
| High | New open port on an existing asset | `port_opened` |
| High | A program tipping en masse into `unobservable`, over 10 % of its assets | `program_unobservable` |
| Medium | Technology or redirect chain change | `tech_changed`, `chain_changed` |
| Medium | Asset gone `INACTIVE` | `went_inactive` |
| Medium | Asset in an unusual geography for the program | `geo_anomaly` |
| Low | Certificate or title change | `cert_changed`, `title_changed` |
| Low | Detection improved: a pure addition after a fingerprinter version bump | `detection_improved` |

Every notification carries the **diff**, before and after, and the
[lineage](/architecture/data-model/#44-lineage). Not just the current state.

A mass tip into `unobservable` usually signals an IP ban on the scanning side rather than a change in the
targets, which is why it is high priority and why it is a program event rather than an asset one.

### Certificate Transparency adds no `kind`

A candidate is not a finding. Most of them resolve to nothing and end
[`ARCHIVED`](/architecture/lifecycle/#66-an-asset-that-was-never-alive), and notifying on creation would
produce the exact flood [12.4](#124-aggregation-and-anti-flood) exists to stop, on the one source that
can deliver several thousand names in a minute.

What is worth saying is that one went **live**, and that is `new_active`, which the table above already
carries and which the lineage already attributes to `certstream`. The event that matters was written
before the source that produces it, so the source arrives with nothing to add.

### What `external_host_dead` can actually see

An external host is **out of scope by definition**: it belongs to someone else. It gets no due date, and
nothing ever probes it. So its death cannot be observed, for lack of ever having looked.

What is detectable is a cross reference: a host appearing in an asset's `external_hosts` and **present in
the same organization's inventory** as an asset the lifecycle has declared dead. That is the internal case,
a home made CDN, a static bucket, a build domain, dying while pages keep loading scripts from it. The vector
is the takeover one, except the target is already referenced by a production page, which removes the step
where an attacker has to get someone to visit.

The other case, a genuine third party whose domain expired and anyone can re-register, is the classic supply
chain one and the more dangerous of the two. Covering it means **resolving names the organization does not
own**.

**A DNS lookup is allowed outside a scan authorization.** What
[11.3](/architecture/security/#113-other-guardrails) protects is **interaction with a third party's
infrastructure**; a query to a public resolver does not reach the domain and imposes no load on it. That is
the same reasoning that gives a resolution no cost against the program's budget.

**The limit is strict, and it is what makes the permission defensible:**

| Operation | On a third party host |
|---|---|
| Resolving the name and its apex | **allowed** |
| TCP connection | forbidden |
| HTTP request | forbidden |
| Render | forbidden |

The test reduces to existence, and the signal is an **`nxdomain` on the apex**: a name that no longer
resolves under a domain that is still registered is a dangling subdomain at somebody else's, which is not
re-registrable and not the same finding.

**The third party host does not enter the inventory.** It stays a property of the asset that references it,
with no `asset` row and no due date. Creating one would mean scheduling checks against a domain nothing
authorizes probing, which is doing by the side door what is forbidden head on.

### Where each half runs

**The internal half is ingestion's, and it works in both directions.** A render projects `external_hosts`,
and the reference can become dangerous from either end: the page starts pointing at a host that is already
dead, or a host that pages already point at dies. Only the first is visible when the referencing asset is
written, and only the second is visible when the referenced one dies, so covering one direction covers
roughly half the cases and produces a finding nobody can predict the shape of. Both are one statement, one
on the render that writes the list and one on the transition into `inactive`, and both stay inside the
ingestion transaction where every other event is produced.

**The external half cannot be, and that is a stated exception rather than an oversight.** It needs a DNS
answer about a domain nothing in an ingestion has any reason to have asked for. The two arguments of
[12.3](#123-where-the-event-is-produced) are what decide it, and neither points the same way here: a sweep
is refused because it would **re-derive** what ingestion just computed, and nothing in an ingestion computes
whether somebody else's domain is still registered; and a sweep is refused because it **misses transient
states**, and an expired registration is not transient, it is a step that stays until someone re-registers.

So it is a loop, on the system pool because it serves every tenant in one tick, and it is bounded by what it
is allowed to do:

- It resolves the **apex** and only the apex, once per distinct apex per tick, never the host itself. That
  is one lookup for every third party a whole inventory references, which is dozens rather than thousands.
- It skips any host that is **in the same organization's inventory**, because that one is the internal half's
  and has a real lifecycle behind it.
- It skips **archived** assets, for the reason pivots do: an archived asset is not a lead.
- It walks the whole set in pages rather than capping the tick. A fixed cap over a fixed order sweeps the
  same rows every time, so everything past it is permanently invisible and nothing says so, which is the
  silent truncation the export and the timeline both refuse.

**A lookup that failed is not an answer in either direction.** It is not "the domain expired", which is
obvious, and it is not "the domain is fine" either, which is the half that gets missed: rewriting the list
without an entry it could not check drops the finding, and the next tick that does resolve reads the same
host as newly dead and re-sends a critical alert about something that has been dead all week. Whatever the
last tick concluded stands until a tick concludes otherwise.

**A verdict that says what it already said is not written.** In steady state that is every asset carrying
an external host, and rewriting them all would be one transaction and one dead row version each, every
tick, to store what was already there.

**The verdict is written onto the referencing asset, in `attributes`, and never into a table of its own.**
That is what keeps the sentence above true: the third party has no row, no identity and no due date, it is a
property of the pages that point at it. It also makes the emission idempotent without a second mechanism,
since the event is produced when a host **enters** that list and the tick that finds the same domain still
gone writes the same value and says nothing.


## 12.3 Where the event is produced

**The event is written in the ingestion transaction, by ingestion itself.** Not by a periodic sweep of the
Notifier.

Same reasoning that put the lifecycle transitions there
([6.2](/architecture/lifecycle/#where-the-transitions-live)), and it holds identically. A sweeper would have
to **re-derive** what ingestion just computed: the transition, the failure qualification, the version
classification, and the comparison of the two payloads.

Second reason, independent of the first: **a sweep misses transient states.** An asset going
`ACTIVE → FLAPPING → ACTIVE` between two passes has changed nothing from the sweeper's point of view, and a
flap is itself a signal.

What stays outside the transaction is the Notifier proper, the part that reads events, aggregates and sends.
It can lag without consequence. It is the **sending** that is asynchronous, never the production of the
event.

```sql
CREATE TABLE notification_event (
  id           bigserial,
  org_id       uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
  program_id   uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
  -- NULL on a program event, which no asset carries: the mass tip into
  -- unobservable, an incident, and the summary.
  asset_id     uuid REFERENCES asset(id) ON DELETE CASCADE,
  kind         text NOT NULL,
  priority     text NOT NULL,
  -- The diff and the lineage, frozen at write time.
  payload      jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  notified_at  timestamptz,
  suppressed   boolean NOT NULL DEFAULT false,

  PRIMARY KEY (id, created_at),

  -- The nullability of asset_id is a rule, not a permission. Without this it
  -- becomes an open door to malformed events: a takeover with no asset, or a
  -- summary claiming to designate one.
  CONSTRAINT notification_event_scope_matches_kind CHECK (
      (kind IN ('program_unobservable', 'run_never_completed', 'digest'))
      = (asset_id IS NULL)
  )
) PARTITION BY RANGE (created_at);

-- The Notifier's queue. `suppressed` is in the predicate, not only in the
-- query: without it a first run leaves a few thousand rows that nothing will
-- ever notify and that every tick rereads.
CREATE INDEX notification_event_pending_idx
    ON notification_event (program_id, priority, created_at)
    WHERE notified_at IS NULL AND NOT suppressed;
```

`kind` and `priority` are `text` with a named `CHECK`, never an enum. Both lists are still moving.

The composite primary key has a consequence on the drain: the Notifier marks its rows by
`(id, created_at)` rather than `id` alone. It holds both, having just read them; forgetting them would scan
every partition on each mark.

**The payload is frozen at write time.** A notification must reflect the state at the moment of the event,
not at the moment of sending. A Notifier ten minutes behind rereading `asset_current` would describe
something other than what it announces.

### The cost, which decides the shape

Ingestion has a round trip budget per observation, and a test fails past it. One `INSERT` per changed
observation would blow it on a first run, where everything changes, which is exactly the scenario the
milestone measures.

So the write travels inside what already exists:

1. The asset upsert returns the **lineage** in passing. It is in the row it just wrote, so it is free, and
   it is what the notification has to carry.
2. The observation write returns, **alongside the deduplication flag, the previous payload and its
   `last_producer_version`**, the two arguments [8.7](/architecture/verification/#87-dating-the-instrument)
   asks to compare. A CTE reads the head of the chain on the pre-insert snapshot, so it costs no extra
   query.
3. The diff and its classification are computed **in Go**, where the arithmetic is testable.
4. The events of one batch are inserted in **one multi-row statement** at the end of the transaction. The
   budget is counted per observation beyond the fixed cost of a batch.

**The previous payload comes back only when there was an insert**, never on the deduplication path. Without
that condition every observation would drag its own payload across the wire while 95 % of them deduplicate
and have no diff to compute.

### Who writes what

| Producer | Events | Reason |
|---|---|---|
| Ingestion, in the transaction | everything carrying an `asset_id` | it already holds the shape of the observation |
| Notifier, on its tick | `program_unobservable`, `digest` | aggregate questions no single observation can settle |

The mass tip is the case that shows the boundary. The per program ratio is a proportion over an inventory,
not a conclusion drawn from one observation, and an aggregate is exactly what a sweep is the right tool for.
It is the only thing given to one.

## 12.4 Aggregation and anti-flood

A sliding window per program, per priority class:

| Priority | Handling |
|---|---|
| `critical` | immediate, never aggregated |
| `high` | 15 minute window, at most 20 per program |
| `medium` and `low` | 60 minute window, at most 10 per program |

**Cooldown on `program_unobservable`: one hour.** A mass tip usually signals an IP ban on the scanning side,
which is actionable within the hour; six hours of silence would be silence on an ongoing incident.

**Re-emission by tier.** The cooldown lifts when the ratio crosses a higher threshold: 10 %, then 25 %, then
50 %. A program flagged at 12 % stays quiet at 15 % and speaks again at 30 %. An incident that gets worse has
to say so, even inside its own window.

Past the cap, a **summary notification**: "340 new active assets on target.com", with a link to the filtered
search. The individual events stay in the database with `suppressed = true`, readable and not sent.

**The summary is an event, not a silence.** An overflow must never produce the absence of a notification,
which is how an anti-flood turns into a loss of signal. It is written to the same table, without an
`asset_id`, carrying the priority of the window it replaces.

:::caution[An event with no asset is never aggregated]
Two things escape the windows: `critical`, which the table already says, and **any event carrying no
`asset_id`**.

A program event is **already an aggregate**: a summary speaks for a batch, a mass tip speaks for an
inventory, and each is emitted at most once per program per window. Folding them into a second aggregate
counts them twice and, worse, loses them. Twenty `new_active` saturating the high window would otherwise
swallow the program that just went dark into their summary, where a distinct alert is exactly what is
wanted.

The rule is written as "carries no asset" rather than as a list of types, so a future program event inherits
it instead of having to remember.
:::

An event with no asset does not consume the window either. Counting it would shrink the cap at the precise
moment a program went dark, which is when the most room is needed.

The window count is read **from the table itself**, the program's rows whose `notified_at` falls inside the
window, and never from state the Notifier holds in memory. An in-memory counter resets on restart, which
reopens the tap exactly when one restarts because of an incident.

**`critical` passes through none of this.** A takeover is notified with at most one Notifier tick between
the event being written and being sent. "Immediate" without a bound is not a testable assertion.

## 12.5 The first run of a program

The first pass over a new program produces one `new_active` per asset, so several thousand. Those events are
written with `suppressed = true` and replaced by a single summary.

Without it, onboarding a program drowns the user and teaches them to ignore alerts, which costs far beyond
the first run.

**The grace holds back `new_active` and nothing else.** Two reasons, and the first would be enough. A
**takeover candidate found during a first run** is exactly the finding this product exists for, and holding
it back because the program is new would be the worst silence the system can produce. And the grace ends on a
completed discovery run or at an asset threshold, so a small program fed by hand never leaves it: everything
suppressed, that program would never notify anything again.

Nothing is lost: a first run produces **no diff**, for lack of a previous observation to compare against, so
`new_active` is the only per asset event it emits in volume. The flood the grace exists to stop is made
entirely of that one type.

**And the suppression covers the assets that were entered, not what is derived from them.** The grace covers
two situations that do not deserve the same treatment. A program in the middle of a first run has a flood, and
all of it is held. A program with no discovery run at all is under grace because its inventory was typed in,
so what is held is what was typed in. An asset discovered by
[Certificate Transparency](/architecture/discovery/#75-certificate-transparency) under a hand entered apex was
typed in by nobody, and it notifies normally. The condition discriminates on `asset.discovery_source`, never
on `program_id` alone.

**The summary is emitted when the grace ends**, not during. Summarizing while the run is going would produce
one summary per batch, each carrying a count already wrong by the time it is written.

**The bound is the program's first discovery run, not a duration.** A grace measured in hours would translate
to "the program is young", when the question asked is "is there already an inventory to compare against". A
program created on a Friday and scanned on Monday has no inventory on Monday.

### The bound, read from the runs

```
grace active while:
    no completed discovery run on this program
  AND ( a discovery run exists on this program, whatever its state
        OR fewer than 500 assets on this program )
```

The information already exists, and a dedicated column would be a denormalization to maintain. A **failed**
first run resolves itself: no completed run, so the grace is still active. A "grace consumed" column would
force deciding whether a failure consumes it, and that question has no good answer.

**The second condition is necessary**: a program fed only by manual entry or by Certificate Transparency will
never have a discovery run and would stay under grace forever. The asset threshold bounds that case.

**And the threshold applies to that case only**, which is not a refinement but what makes the rule correct.
Written as a plain AND, the threshold ends the grace **in the middle of the run it exists for**: a perimeter
of five thousand assets would leave the grace partway through and flood with the rest. A program in the middle
of a first run is not a program without discovery.

**The grace is evaluated when the event is written**, so in ingestion, once per batch. That follows from
[12.3](#123-where-the-event-is-produced): `suppressed` is frozen at the same instant as the payload, for the
same reason. A Notifier deciding at drain time would send a first run's flood late rather than never, the
grace having ended between the write and the send.

:::caution[The grace reads the asset's source, never the observation's]
An observation carries the source of the **probe** that produced it. But the `new_active` of a hand entered
asset is born from a probe observation, so read from the observation, a typed in asset arrives as a probe, the
second branch of the grace never recognizes anything, and it is **dead code in production** while its test
passes, because the test fabricates a value nothing emits.

`ON CONFLICT` does not touch `discovery_source`, so the value read is the one from the first appearance, which
is exactly the question the grace asks.
:::

### An age guardrail

The grace ends at the latest **7 days after the program was created**, whatever the state of its runs. A
grace is an alert suppression mechanism, and its termination must not depend on any other component.

On expiry **without** a completed run, two things go out together: the onboarding summary, and a
`run_never_completed` event at high priority. A program whose first run does not finish in a week is an
incident, and absorbing it in silence is a perimeter quietly ceasing to be scanned.

The age is applied on both sides, since the grace is decided at write time by ingestion and reread on the tick
by the Notifier. Two different values would put a program under grace on one side and out of it on the other.

:::note[The threshold counts assets, not events]
An asset from a discovery run is born `CANDIDATE` and produces no `new_active`. Counting events would stretch
the grace well past the inventory it is meant to cover. What the condition asks is "does this program already
have an inventory", and an inventory is counted in assets.
:::

## 12.6 Outputs

**One generic webhook, and only that, in v1.** URL, method, headers and payload template all configurable.

Discord and Slack are then configuration rather than code: they are webhooks with a particular payload shape.
Writing a Discord connector would freeze in Go what a template expresses, and start over at the next one.

**Email is post-v1.** SMTP brings a dependency, a secret and deliverability problems, for the channel least
suited to this kind of alert.

### Channels belong to an organization

```sql
CREATE TABLE notification_channel (
  id           uuid PRIMARY KEY,
  org_id       uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
  kind         text NOT NULL DEFAULT 'webhook',
  url          text NOT NULL,
  -- The *name* of a secret, never a secret.
  secret_ref   text,
  template     text,
  enabled      boolean NOT NULL DEFAULT true,
  min_priority text NOT NULL DEFAULT 'low',
  managed_by   text NOT NULL DEFAULT 'console'  -- config | console
);
```

Worth doing now despite single tenant use: retrofitting a global output into a per organization one means
reworking the entire send path, which is the shape of decision [11.1](/architecture/security/#111-irreversible-decisions)
classifies as irreversible. The current configuration becomes the table's single row.

`min_priority` routes by criticality without multiplying components: a channel that only wants what is burning
says so here, and not in a filter placed somewhere along the path.

**`secret_ref` names a secret and never contains one.** Secrets stay out of the repository and out of the
database, and a column carrying one would put every tenant's credential behind a single `SELECT`. A channel
whose `secret_ref` resolves to nothing is **refused** rather than called without a credential: an endpoint
expecting authentication answers 401, and the events would pile up against a target the logs do not let anyone
repair.

**The channel derived from configuration carries `managed_by = 'config'`** and is unique per organization.
Without that marker the bootstrap keys on the URL, so changing the configured URL and restarting inserts a
**second** active row without disabling the first, and every alert then goes out twice, one of them to the
destination just replaced.

**Transport settings belong to the deployment**, not to the channel: method, timeout, attempts and backoff
apply to every channel. The URL, the template and the priority floor belong to the row.

**The bootstrap only runs when exactly one organization exists.** A configuration file has no way to name a
tenant, so that is the only case where the intent is unambiguous. Beyond it, giving a global URL to one of
several organizations would leak one tenant's alerts into another's channel.

**An organization with no channel and a broken channel are not the same case**, and confusing them is a way to
lose all of an organization's alerts in silence. With no channel, that is a deliberate configuration: events
are marked delivered and counted by a metric, so "computed and sent nowhere" stays visible. A channel that
**exists** and cannot be built, a `secret_ref` resolving to nothing, a template that does not compile, is an
outage: resolution fails, the events stay queued, and the stuck queue alert does its job.

`notified_at` is set only on a 2xx. That is the one rule stopping a webhook outage from becoming a silent loss
of alerts. Failures are retried with a bounded backoff and counted by a metric: a dead webhook is an
observability outage, not a transport detail.

## 12.7 Volume and retention

`notification_event` grows at the same order of magnitude as `observation` without the benefit of
deduplication: an onboarding writes one row per asset.

**Monthly partitioning** on `created_at`, aligned with
[4.5](/architecture/data-model/#partitioning-from-the-first-migration) and served by the same function. No
`DEFAULT` partition, for the same reason.

**Asymmetric retention**, because the three populations do not have the same value:

| Population | Retention |
|---|---|
| `notified_at IS NOT NULL` | 12 months, this is the alert history |
| `suppressed = true` | **30 days**, onboarding and overflow noise, readable for the length of an investigation |
| `notified_at IS NULL AND NOT suppressed` | never purged, this is the queue |

The 12 month retention runs as a partition `DROP`; purging the suppressed rows stays a targeted `DELETE` inside
the partitions. Do not try to partition on `suppressed`: those rows are written in waves and the `DELETE` is
bounded.

**The third row deserves an alert.** An event neither notified nor suppressed for more than 24 hours signals a
broken Notifier, and that is a silent failure by nature: nothing else announces it, ingestion keeps writing and
the inventory stays correct. It is the kind of outage that goes unnoticed until the takeover it misses.

Draining the queue crosses every partition, which is acceptable for a precise reason rather than out of
indulgence: the queue index is **partial**, so it is empty on any partition whose events have been handled. A
closed month costs an empty index to consult, not a month of rows to filter.

:::caution[Partition maintenance is not a feature]
The function that creates next month's partitions has to be called by a loop that **starts unconditionally**.
Placed inside the Notifier's tick, it inherits a notifications toggle, and turning alerts off would reopen the
outage three months later on `observation`, through a button that talks about alerts. The stuck queue gauge and
the retention purge are in the same case: none of the three is a feature.
:::
