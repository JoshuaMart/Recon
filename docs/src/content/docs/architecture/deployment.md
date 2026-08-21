---
title: Runs and deployment
description: How a run is defined, started, authenticated and ingested, and how privilege is separated across the three runtimes.
sidebar:
  order: 9
---

## 9.1 The run contract

A run is one execution of FastRecon over one perimeter, producing one report. Everything it needs
arrives in its definition, and nothing it holds opens the inventory ([P6](/architecture/principles/)).

| What it receives | Where from |
|---|---|
| The perimeter: an apex, or a target list URL | the run definition |
| Stage scope, port list, exclusion patterns | the run definition |
| A rate limit | `program.rate_limit_rps` |
| Where to post the report, and with what header | the run definition |
| Source API keys | the runtime environment, never the definition ([7.3](/architecture/discovery/#73-source-credentials)) |

A serverless job definition offers environment variables, which is exactly what FastRecon is
configurable through:

```
FASTRECON_TARGETS_URL=https://recon.example/runs/01M0GH.../targets?token=<targets token>
FASTRECON_STAGES=resolve
FASTRECON_PORTS=80,443,8080,...
FASTRECON_SCAN_RATE=10
FASTRECON_WEBHOOK_URL=https://recon.example/reports
FASTRECON_WEBHOOK_HEADER=Authorization: Bearer <run token>
FASTRECON_TIMEOUT=30m
```

The target list is served as plain text, one canonical host per line, because the consumer is a scanner
reading a target file and the format has to be one it already accepts.

**Two signed values, no table.** The targets URL and the report token are HMACs over
`(run_id, purpose, expiry)`, computed with a server key. Nothing to store, nothing to revoke, nothing
to purge, and an expiry that is intrinsic. The targets URL is readable for minutes; the report token
lives as long as the run's deadline plus a margin.

**A discovery run gets no targets URL** and a domain instead. That is the whole difference between the
two mandates of [7.1](/architecture/discovery/#two-inputs-two-mandates).

## 9.2 A run has a row

```sql
CREATE TABLE run (
  id             uuid PRIMARY KEY,
  org_id         uuid NOT NULL REFERENCES org(id),
  program_id     uuid NOT NULL REFERENCES program(id),
  kind           text NOT NULL,   -- discovery | verification
  scope          text NOT NULL,   -- enum | resolve | ports | full
  state          text NOT NULL,   -- pending | running | completed | failed | expired
  deadline       timestamptz NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  started_at     timestamptz,     -- written when a scanner first reaches for the run, see 9.8
  finished_at    timestamptz,
  target_count   int,
  summary        jsonb,           -- stats, per source accounting, warnings
  error          text
);

-- The frozen target list of a verification run. This is the lease.
CREATE TABLE run_target (
  run_id   uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES asset(id),
  key      text NOT NULL,
  PRIMARY KEY (run_id, asset_id)
);
```

**The target list is the lease**, and that is the whole of the reservation mechanism. Selection
excludes assets already listed in a non terminal run, so two runs never scan the same host at the same
time, and the reservation expires when the run does. There is no per asset lease column, no lease
token, no partial restitution and no heartbeat.

**One live run per kind per programme, and the database is what says so.** Selection reads what is held
and then writes what it takes, and no transaction sees another's uncommitted rows, so two overlapping
ticks both find nothing in flight and both freeze the same hosts. A partial unique index on
`(program_id)` for each kind makes the second one lose, which turns the reservation into a fact rather
than the outcome of a check. The refusal a caller sees is the same either way.

**A run that dies takes nothing with it.** Due dates are moved only when a report is ingested, so an
abandoned run leaves the inventory exactly as it found it and the next tick selects the same assets
again. A sweeper marks runs past their deadline `expired`, which frees their targets and makes the
failure visible; it does not have to repair anything.

**A run in a terminal state rejects any later report bearing its id.** That is the effective
revocation, since a signed token cannot be recalled: it stays valid and stops being useful.

## 9.3 Ingesting a report

`POST /reports` carries the report document and the run token. Ingestion, in order:

1. **Authenticate the token**, then read the run id from its claims. Reading the id from the body first
   would let anyone probe an organization's run states by the shape of the answer.
2. **Check the run is not terminal**, and that its program is still authorized.
3. **Reject hosts outside the frozen list**, on a verification run. Out of list is rejected, never
   silently ignored: that is what stops a scanner choosing its own perimeter. A discovery run has no
   list, and hosts outside the scope are accepted and classified `unknown` or `out_of_scope`, which is
   the point of [5.3](/architecture/scope/#53-three-states-not-two).
4. **Write the observations**, recompute [`outcome` and `usable`](/architecture/verification/#outcome-and-usable-are-orthogonal),
   apply the transitions, derive the [services](/architecture/verification/#an-open-port-becomes-an-asset).
5. **Reschedule** the hosts the report answered for, and close the run.

**Hosts the report does not mention get nothing.** No observation, no rescheduling, no counter. FastRecon
returns them as `discovered` when a deadline cut the run short, and the report says so in `completed` and
`truncated_by_timeout`. Silence is not a measurement, and turning it into one is the single most
expensive mistake available here.

**A report that says it ran degraded is accepted and downgraded.** Truncation, a wildcard sweep that hit
its cap, a resolver pool that could not be validated: each is a run stating what it could not guarantee,
and [6.4](/architecture/lifecycle/#a-degraded-run-cannot-conclude-a-death) turns that into a rule about
what its observations may conclude. Ingestion reads those signals before it qualifies anything.

**A late report is accepted and marked as such.** The data is still valid; the run may simply have been
re-dispatched, and deduplication merges the two. Delivery is idempotent on the run id: the same report
posted twice writes one series of observations.

**A run that delivered a report is `completed`, whatever its scope reached.** Running out of time is data
rather than an error, and a scheduler reading `failed` would re-run work whose results it already holds.
`failed` and `expired` belong to a run that delivered **nothing**, and the deadline sweeper owns those.
What a run said about its own completeness is recorded beside what it wrote, so nine hundred hosts
delivered before a deadline stay distinguishable from a crash on the first.

**A report whose schema major version is unknown is refused.** The report type is transcribed rather than
shared so the scanner can evolve on its own cycle, and that only holds if a document reusing field names
under new meanings is a refusal. A minor bump adds fields, which the unknown-field counter already
handles.

FastRecon delivers the whole document as raw JSON, retrying on 5xx, 429 and transport errors with
exponential backoff and jitter. Its exit codes distinguish "report delivered, scope unfinished" from "no
report produced", and only the latter is worth retrying, so the job definition leaves platform retries
off.

## 9.4 Separating privilege, not just load

| Component | Runtime | Privileges | Why |
|---|---|---|---|
| Control plane | permanent host | no raw network | small, stable, never scans |
| FastRecon run | serverless job | **none** | connect scanning needs no root |
| Fingerprinter | long-running service | none, isolated network | headless browser, persistent state, filtered egress |

The key point: **connect scanning throughout** is what unlocks serverless everywhere. There is no SYN
scan, so no `CAP_NET_RAW`, no privileged ephemeral instance, and one runtime profile for every scan the
platform performs. A privileged runtime by default is a runtime nobody remembers to de-privilege.

Raw SYN scanning is only worth it for sweeping large address ranges. On cloud targets, which dominate in
practice, a high concurrency connect scan gives the same answer.

The hidden benefit: the control plane never needs network privileges, and the scanners never get database
access. That is the security boundary required to open the platform to anyone else.

## 9.5 Rate limiting

The budget belongs to the **program**, not to one scanner. Twenty uncoordinated scanners saturate a target
and get addresses banned.

FastRecon carries its own limiters, a scan rate for the port sweep and a request rate for the HTTP probe,
both set from `program.rate_limit_rps` in the run definition. Concurrency is bounded to a small number of
runs per program by [9.2](#92-a-run-has-a-row), so a per process ceiling is effectively the program's
ceiling.

One cost is worth knowing because it is invisible in the port count: the
[certificate pivot](/architecture/search/#105-pivots) costs a second TLS handshake per HTTPS service,
since the HTTP client hands back the certificate's parsed fields rather than the key. It is a run level
flag, so a pass that does not need the pivot does not pay for it.

**That is a limitation, and it is worth naming rather than discovering.** It is a process ceiling, not a
shared bucket. It holds because concurrency is bounded, and it stops holding the day it is not. A shared
token bucket comes back at that point, and not before.

Renders are the other consumer, and the control plane meters them in memory, per program. **A render does
not cost one request.** A browser fetches the page, then its same host subresources, scripts, stylesheets,
images, the XHRs the page issues, then the 404 probe, `robots.txt`, the sitemap and the favicon. Thirty is
the order of magnitude of a real application page. Third party subresources go elsewhere and cost the
target nothing, which puts the figure below the browser's total request count and makes it a setting
rather than a constant. Billing it as one would make the most expensive thing in the system the cheapest
on the counter.

The cost is **fixed**, and two plausible modulations are not. A screenshot adds no request: it photographs
a viewport the browser already rendered. And a first baseline is no colder than any other scan, since the
service opens a fresh browser context every time.

The in memory meter assumes **one control plane process**. A second one needs a shared bucket, and that is
the condition under which a shared token bucket store comes back into the deployment.

## 9.6 PostgreSQL roles

| Role | Use |
|---|---|
| `asm_owner` | owns the objects, runs migrations and seeds. **Never** used by the application |
| `asm_app` | used by the control plane. DML on the business tables, `SELECT` only on the seeded tables, no DDL |
| `asm_sys` | the same privileges plus `BYPASSRLS`. Reserved for the background loops that serve every tenant in one tick, and never reachable from a request |

Three justifications, two of which go well beyond tidiness:

1. **Row-Level Security does not apply to a table's owner.** An application connected as owner would make
   enabling RLS silently inoperative, which is the worst possible failure mode for an isolation mechanism:
   it signals nothing.
2. **It shrinks the blast radius of an SQL injection.** A role without DDL cannot drop a table.
3. **A `REVOKE` only has an effect with two roles.** An owner keeps its privileges and can grant them back
   to itself.

**Why now.** Retrofitting roles means reassigning ownership of every existing object, which is tedious and
risky on a live database.

Two implementation traps:

```sql
ALTER DEFAULT PRIVILEGES FOR ROLE asm_owner IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO asm_app;
ALTER DEFAULT PRIVILEGES FOR ROLE asm_owner IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO asm_app;
```

Without default privileges, every new table needs a manual `GRANT` that will be forgotten. Without the
sequence grant, `bigserial` columns fail in a way that is hard to diagnose.

**The schema knows nothing about authentication.** The migration creates `asm_app` as `NOLOGIN` with its
privileges. Granting `LOGIN` and a secret belongs to the deployment entrypoint, never to a migration file.
The method varies, and the schema has no business knowing it.

**Partitioning goes through a `SECURITY DEFINER` function.** The scheduler runs as `asm_app`, which has no
DDL, while creating a partition is a `CREATE TABLE`. The maintenance function therefore executes with its
owner's rights, with a fixed `search_path` and `EXECUTE` granted to `asm_app` alone. The drop function is
**not** granted: purging is a maintenance operation, run as the owner.

**`ANALYZE` needs the `MAINTAIN` privilege**, granted to `asm_sys`. Ordinarily `ANALYZE` requires table
ownership, and run by a non owner it is **skipped with a warning, never an error**, which is how stale
statistics stay invisible. That matters more than it sounds: on two joined tables of very unequal size,
stale statistics on one produce a catastrophic plan on both, and the planner will pick a nested loop over a
million rows without anything failing. The control is `n_mod_since_analyze` against `reltuples`, which
PostgreSQL maintains for free, and the housekeeping loop runs the `ANALYZE` itself past a threshold rather
than waiting for autovacuum.

### The connection strings

| Variable | Role | Use |
|---|---|---|
| `RECON_DATABASE__MIGRATION_URL` | `asm_owner` | migrations, seeds, bootstrap. Outside the runtime |
| `RECON_DATABASE__URL` | `asm_app` | control plane requests |
| `RECON_DATABASE__SYSTEM_URL` | `asm_sys` | background loops |

The control plane opens two pools, and that is deliberate: the role that crosses tenants is chosen when a
pool is opened, not case by case in the code, otherwise the property goes back to being a convention.

The migration string must **never** be present in the control plane's environment. If it is, role
separation buys nothing, since anyone obtaining execution in the control plane finds the owner credentials
next to it. The separation has to be a fact of deployment rather than a naming convention, so the
configuration **refuses to start** when both appear, and the migration string lives in the release job
rather than in the service definition.

The refusal is symmetric: the migrator refuses the application credential too. Neither process has any
business holding the other's.

## 9.7 Bootstrap

```
recon bootstrap --org "Name" --email person@example.com
```

Creates the organization, the user, the membership and an **initial token**, printed once since only its
hash is stored.

**A command and not an endpoint**, which is the decision that matters. It runs as `asm_owner`, through the
migration string, therefore **outside the application path**: the control plane does not hold that
credential and has no way to create an organization. An endpoint for creating organizations would
necessarily be unauthenticated, since there is no tenant to attach the caller to before one exists, and
that is the classic hole in this spot.

It is **idempotent on the organization name**. Replayed, it does not create a second one; it issues a new
token if asked. A bootstrap that silently duplicates is what produces two tenants with the same name, one
of them empty.

A system whose bootstrap goes through hand written SQL is not deployable, and it makes multi-tenancy
untestable: creating a second organization to check isolation would need the same intervention, so nobody
does it without being told.

## 9.8 Starting runs

### What actually starts one

**The control plane never starts a container.** Doing so needs a socket that grants root on the host, in the
process that already holds the database credentials. That is a worse privilege than the one connect scanning
removed, and it would be acquired for convenience rather than for a capability.

The scanner is deployed **once**, as a job definition, out of band. Starting a run is a single API call that
starts that definition while **overriding its arguments and environment for that run**, which is what lets
one generic definition serve every program: the perimeter, the stage scope, the port list, the targets URL
and the report destination are all overrides.

Four consequences, and the first is the one that bites in silence.

**The control plane starts, it never updates.** The API call that modifies a definition **replaces** its
whole environment map rather than merging into it, so a control plane that wrote its run definition that way
would wipe the source API keys the definition carries. Nothing would fail: the next run would simply query
fewer sources and find less, which is the exact failure mode
[7.3](/architecture/discovery/#73-source-credentials) exists to make visible. Credentials live on the
definition, and the definition is somebody's deployment, never the control plane's write path.

**Its credential is scoped to starting definitions**, and to nothing else on the account. It sits in the
process that already holds the inventory, so the only thing bounding the damage of that process being
compromised is how narrow this key is.

**The timeouts nest, and Recon owns only the inner one.** The run's own budget has to expire before the
platform's, so that a long run still delivers a truncated report instead of being killed with nothing. The
outer bound is on the definition, which Recon does not control, so the inner value is configuration and has
to be set to match. The `deadline` on the run row derives from the same value.

**Platform retries stay off.** A scanner distinguishes several exit codes and only one of them is worth
retrying, which a platform that sees only "non-zero" cannot single out. Retrying the others re-runs work
whose report was already delivered. Recon's retry is the due date, which is the same mechanism as everywhere
else: nothing moved, so the next tick reselects.

**When the platform refuses**, on a quota or an outage, the run row stays `pending` and the deadline sweeper
expires it. The signed token and the frozen target list expire with it, and the due dates were never moved,
so the next tick starts a fresh run over the same assets. Nothing has to be repaired, which is the property
[9.2](#92-a-run-has-a-row) is built around.

**The spend ceiling stops being theoretical here.** A run now costs money per execution, so the cap on
concurrent runs per program is what bounds a bill rather than a queue. A bound that has never bound anything
has never been calibrated either, and this is the moment it starts mattering.

**In development there is no platform.** The control plane renders the run definition and the console shows
it, and a person runs the image with it. That is the same shape as production minus the call, which is what
keeps the local path from becoming a second way of starting a run.

### When one starts

Two paths, complementary rather than alternative: scheduled for regular coverage, manual to re-run after a
scope change.

The scheduler makes a pass for discovery, distinct from the one on due dates:

```sql
WHERE p.state = 'active'
  AND p.authorized_from <= now()
  AND (p.authorized_to IS NULL OR p.authorized_to > now())
  AND (p.last_discovery_at IS NULL
       OR p.last_discovery_at + p.discovery_interval <= now())
  AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id AND r.kind = 'discovery'
           AND r.state IN ('pending', 'running'))
```

**The authorization window is checked here**, not just `state`. Without those two lines an expired program
would be provisioned on every tick and refused when the run opens: an execution billed to do nothing,
every thirty seconds.

**The absence of a run in flight is the condition that does the work.** It prevents a provisioning storm,
since `last_discovery_at` is written when the run is created rather than when it completes, and it bounds
concurrency to one discovery run per program.

`last_discovery_at` is written **at creation**, not at completion. A run that dies on the way must not be
restarted by the cadence: the deadline sweeper already handles it, and confusing the two would start two.

**Console endpoint**: `POST /programs/{id}/runs`, needed to re-run after a scope change, independently of
the cadence.

### What a refusal has to say

A second request while a run is in flight is a 409, and the message has to name the run, its state and its
age, because two situations there call for opposite actions:

- a run something has actually opened is a run to wait for;
- a run nobody has claimed is a run whose provisioning failed.

What separates them is `started_at`, written the first time a scanner reaches for the run. On a
verification run that is the fetch of its target list, and on a discovery run, which has none, it is the
report. It is the only thing that says a scanner actually opened the run rather than a provisioner having
promised to, and taking it from the report alone would have left every verification run that died mid
flight looking like one nothing ever started.

## 9.9 Reading the queue

Queue depth is measured on every scheduler tick and goes into a gauge that nobody looks at. The question
asked in front of a console is not "how deep is the queue" but **"why is nothing moving"**, and answering
it should not require a `psql` session.

`GET /queue`, on the console credential, under a read action a scanner does not hold. The queue is read
from a console and never from what consumes it.

| Number | What it counts |
|---|---|
| `due` | `next_*_at <= now`, not in a live run: what the next tick can dispatch |
| `later` | `next_*_at > now`: scheduled, not yet due |
| `in_run` | listed in a non terminal run: held by an execution in flight |

**Rows with no due date are counted nowhere**, and that is the only choice that does not lie: a null is how
a row leaves the scheduler, so it is an archived asset. Filing them under `later` would show a queue that
never drains.

The response also carries the organization's most recent runs, with their state, age, observation count and
error. Without them, an empty queue and a full one look alike: in both cases the screen shows a number and
nobody knows whether anything is running.

**No list of the assets at the head of the queue.** It would be read through the dispatch path itself, and a
page replaying that on every load competes with what it claims to observe. **No history** either: the queue
is a present state, not a series, and a series needs a time series store, which is what the gauge is for.
