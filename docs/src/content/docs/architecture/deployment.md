---
title: Runs and deployment
description: How a run is defined, started, authenticated and ingested, and how privilege is separated across the three runtimes.
sidebar:
  order: 9
---

## 9.1 The run contract

A run is one execution of FastRecon over **one root domain**, producing one report. Everything it needs
arrives in its definition, and nothing it holds opens the inventory ([P6](/architecture/principles/)).

One domain and not one perimeter, and the distinction is the scanner's rather than a preference here.
`-d/--domain` is a single value where `--exclude` and `--targets` are repeatable: the flags that accept
several say so. A programme whose perimeter declares three apexes therefore gets **three discovery runs**,
one each, and the run row carries the apex it walked so a failure among them is attributable. This was
learned the way these things usually are: the scheduler joined a programme's apexes with a comma into that
one flag, every single apex programme survived it because a list of one has no separator in it, and the
first perimeter with two produced `domain "a.test,b.test" is not a valid domain name` and a run that died
before it resolved anything.

| What it receives | Where from |
|---|---|
| The perimeter: an apex, or a target list URL | the run definition |
| Stage scope, port list, exclusion patterns | the run definition |
| A rate limit | `program.rate_limit_rps` |
| Where to post the report, and with what header | the run definition |
| Source API keys | the runtime environment, never the definition ([7.3](/architecture/discovery/#73-source-credentials)) |

A start call overrides the definition **for one run**, and the two halves of that override behave
differently. This was measured against the platform rather than assumed, and it decides the shape of
everything below:

| | |
|---|---|
| `args` | **replaced** wholesale by what the start call sends |
| `environment_variables` | **merged** into what the definition already holds |

The merge is what the design needs: the definition keeps the source API keys
([7.3](/architecture/discovery/#73-source-credentials)) and the control plane sends only what it owns.
The replacement is the trap. **A flag on the definition beats a variable sent at start**, so a definition
carrying `-d hackerone.com` makes every run scan that, whatever the environment says, and nothing looks
wrong. So the whole invocation travels as arguments:

```
--stages resolve
--targets-url https://recon.example/runs/01M0GH.../targets
--targets-header "Authorization: Bearer <targets token>"
--ports 80,443,8080,...
--scan-rate 10
--webhook-url https://recon.example/reports
--webhook-header "Authorization: Bearer <run token>"
--timeout 30m
```

**`--stages` takes a scope name**, not a list of stages: `enum`, `resolve`, `ports` or `full`, each rung
running the ones below it. They are the same four values `run.scope` is constrained to, so the column and
the flag cannot drift apart. Inventing a stage list there is a run that fails on its configuration a
second after it starts, and it is invisible to a test suite where both sides of the invention live in the
same repository.

**Neither credential goes in a URL.** The target list is fetched with the signature in a header, which is
what the flag exists for: a token in a query string is a token in every access log, proxy log and error
message that ever prints it, and those outlive the run by a long way. The list itself is plain text, one
canonical host per line, because the consumer is a scanner reading a target file.

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
  kind           text NOT NULL,   -- discovery | verification | candidate
  scope          text NOT NULL,   -- enum | resolve | ports | full
  apex           text,            -- the one root domain a discovery walks; null on the others
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

**Discovery is bounded on `(program_id, apex)` instead**, because that is the unit it holds: a run
enumerates one root domain ([9.1](#91-the-run-contract)), so a perimeter of three apexes has three
enumerations to provision and one of them being in flight says nothing about the other two. The `apex`
column is what makes that expressible, and it is also what makes a failed enumeration attributable:
without it three identical pending rows name nothing. A `CHECK` ties it to the kind in both directions,
since a discovery with no domain cannot say what it walked and a verification with one is claiming a
mandate it does not have.

### A third kind, so a candidate never waits behind a sweep

A Certificate Transparency candidate is due one minute after it is created, and the whole of the
[aggressive curve](/architecture/lifecycle/#backoff-curves) rests on that first check happening then. The
bound above breaks it as written: a verification run over a program's due names can hold the slot for its
whole deadline, so a candidate arriving a minute in waits half an hour for a check the curve wanted at
sixty seconds. That is the differentiator being spent on a queue.

So `candidate` is a **third value of `kind`**, with its own partial unique index. The mechanism was
already per kind, which is what makes this a value rather than a second reservation scheme: one index,
one selection, and nothing else in this section changes.

Two bounds keep it from being a hole in the rate limit rather than a lane in it:

- **Its scope is pinned to `resolve`.** One round trip to a resolver pool per name, and nothing at all is
  sent to the target, so a candidate run and a verification run in flight on the same program do not add
  up to anything [9.5](#95-rate-limiting) has an opinion about. A candidate run may not be widened to
  `full` by any caller: the expensive rung is the next section's business.
- **Its deadline is minutes**, not the run timeout. It does one rung over a short list, and a slot held
  for thirty minutes by a run that had one thing to do turns the bound back into the problem it solves.
  It is its own setting, and the configuration refuses a value above the sweep's: a lane whose slot is
  held longer than the one it exists to avoid waiting for is that wait wearing a different name.

**The expensive rung goes back into the ordinary lane.** A candidate that answers earns `next_full_at`
and is selected by the verification pass with everything else, where one live run per program is exactly
the bound a hundred connections per host deserves. That date is also what takes it out of this lane, so
the promotion and the hand over are one write rather than two facts that could disagree.

**Nothing else holds a candidate run back.** A live verification does not, which is the point. A live
discovery does not either, and that is a decision rather than an oversight: discovery blocks verification
because a full sweep sends a second scanner at hosts the first one is connected to, and a `resolve` sends
nothing to the target at all. What still holds it is another candidate run, kept by the index, and the
frozen list, which is one lease across the lanes rather than one per lane.

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

Which path takes which pool is the whole specification of the second one, so it is written here rather than
inferred from the code:

| Pool | What runs on it |
|---|---|
| `asm_sys` | the partition and housekeeping loop, the deadline sweeper, the discovery cadence, the due date pass, the candidate pass and its own selection, the Certificate Transparency set reload and its apex counters, the Notifier, the render pass, the external host sweep, and the two lookups that turn a credential into an organization, one per kind of credential |
| `asm_app` | everything a request does after that lookup, including the whole of report ingestion |

The Certificate Transparency reload is on that list for the same reason as the lookups rather than for the
same reason as the loops. One socket serves the whole deployment, and the apex set is the thing that decides
which organization a name belongs to, so it is a query that *discovers* a tenant and cannot be filtered by
one. What it hands back is then written scoped, on the application pool, under the organization the matched
apex named.

The credential lookups are on the system pool for a reason that is not convenience: they are the queries
that *discover* the tenant, so they cannot be filtered by one. That is argued in
[11.1](/architecture/security/#row-level-security-two-roles-rather-than-one-variable), and they are the only
members of that column reachable from a request. There are two because there are two kinds of credential: a
console token names itself, and a run token names a run.

**The two pools have two connection budgets.** One setting read twice means a deployment tuned so the
control plane never exceeds a given number of backends quietly opens twice that, which on a managed
instance with a hard `max_connections` is how the *next* service fails to connect. The system pool is
bounded much lower, because it serves a handful of loops on a timer rather than every request.

**Readiness probes both.** The system pool is what turns a credential into an organization, so a broken one
answers every authenticated request with a 500 while a process checking only the other keeps reporting
itself ready and stays in the rotation.

**Every acquisition of the application pool goes through one helper that opens a transaction and sets the
organization**, and there is no way to get a bare connection out of it. A `SET LOCAL` is transaction scoped
by design, so a query outside a transaction cannot carry one, and an API that hands out both shapes is an
API where the wrong one gets used at some point and returns zero rows instead of failing.

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

It is **idempotent**, and the key is the **email** rather than the organization name. That correction is
worth the two lines it costs: `app_user.email` carries a UNIQUE constraint in the schema and `org.name`
deliberately does not, because two customers may legitimately be called the same thing. Keying on the name
would be this command hoping to be unique where the database enforces nothing, and the failure is the one
the idempotency exists to prevent: a second tenant with the same name, empty. So the person is the anchor,
the organization is reached through the membership, and a replay finds both. It issues a new token if
asked, which on a replay is the only reason to run the command at all since the first secret was printed
and never stored.

The four rows commit **together**. A half bootstrapped tenant is worse than none, because the next run
finds the organization and stops before creating what is missing.

It is a subcommand of an operational binary rather than a binary of its own, and that is where the rest of
this sort of thing goes. `migrate` stays separate: a failed migration has to be readable on its own, and
the deployment gates the application on it.

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

**The run is committed before anything is started**, and that order is the whole of the failure story.
Starting inside the transaction would leave an execution running with a valid credential against a run row
that was rolled back.

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

### The pass on due dates

The counterpart of the one below, and the two answer different questions. Enumeration asks what exists under
a perimeter and runs on the program's own interval. This asks what still answers, and it runs on the assets:
a due date is written by every ingestion, and this is what turns it into a run.

It selects the active, authorized programs holding at least one **name** that is due, and starts one
verification run for each. A name is the unit, because an address is never a target of its own and a service
is observed through its host's run.

**One run per program, holding every due name of it.** The frozen list is what makes that possible, and the
alternative is one execution billed per name.

**`full` before `resolve`.** A full run executes every rung below it and its report moves both dates, so
taking full first cannot starve resolve, and an asset due for full does not need a resolve run.

**The interval is how fast the pass reacts, not how much it starts.** What bounds the runs is the due dates
and the one live verification run per program, which is a unique index rather than a check. A tick that
finds nothing due starts nothing, so a short interval costs a query rather than an execution. Short is what
makes a declared asset go out at once, since [entering one](/architecture/scope/#entering-an-asset-by-hand)
writes a due date of now: somebody is waiting.

**No verification while an enumeration is walking the same perimeter**, and the frozen list cannot express
that. A discovery run freezes nothing, because it is the one allowed to find things, so the lease has no way
to see the hosts it is scanning. The window is the run itself: its report has not landed, every asset it
touches still carries the due date it had before, and a verification starting a minute in sends a second
scanner at hosts the first one is connected to. The check sits at the definition rather than only in the
selection, because the console reaches it directly and never passes the selection.

**It is one directional.** Discovery is rare and its cadence is a promise, so it wins; verification
re-selects on the next tick and loses nothing but the length of a run. The symmetric rule would be a
starvation: during a drain a verification is in flight most minutes, and the enumeration would never go out.

**What it does not cover** is two programs whose perimeters overlap. The bound is per program, so the same
name held by two of them is two assets and two runs, and nothing joins them. Saying so is cheaper than
discovering it.

**The authorization window is checked in the selection as well as at the run**, and what the first one buys
is worth stating exactly. Nothing is billed either way, because the run is refused before the platform is
called. What it prevents is the pass waking an expired program, walking its due assets, freezing nothing and
logging a refusal, once a minute, forever. A warning a minute is not a signal.

**There is no cap on concurrent verification runs per organization**, and that is a decision rather than an
omission. The bound today is one per program, which on a deployment holding a handful of them is the bill.
It becomes worth having the day programs multiply, and a bound that has never bound anything has never been
calibrated either.

### The pass for candidates

The third pass, and it selects on the same column as the first: names due to `resolve` whose lifecycle is
`candidate`, on programs holding no candidate run in flight. One run per program, holding every due
candidate of it, scope `resolve`.

**A candidate is selected by this pass and by no other**, and the exclusion is mutual. Without it the two
passes fight over the same names: each freezes what the other was about to take, and which one wins is
whichever tick fired first.

**What the lane holds is the pair `candidate` and no full date, never the lifecycle alone.** `CANDIDATE` is
not a Certificate Transparency state: a host somebody typed into the
[assets form](/architecture/scope/#entering-an-asset-by-hand) is one too until its first answer. Selecting
on the lifecycle alone diverts it into a lane pinned to `resolve`, where a resolution reports that the name
answers and nothing ever sweeps its ports, which is the opposite of what a typed name is promised and it is
promised it because a person is waiting. A null `next_full_at` is what separates them, and it is exactly the
state the aggressive curve describes: a candidate that has not yet earned the expensive rung.

**It is not a single host run, and that phrase was doing work it should not.** What makes a candidate's
first check cheap is the [targets input](/architecture/discovery/#72-how-a-verification-run-gets-its-targets):
stage 1 is replaced, so there is no enumeration and no source quota spent, whatever the list holds. A
certificate carrying eighty nine names, the widest in a measured sample of four thousand, therefore
produces **one** run rather than eighty nine, which is the same grouping the other two passes do and the
only shape that survives a burst. One host is the common case, not the mechanism.

### The pass for discovery

The scheduler makes a pass for discovery, distinct from the one on due dates above:

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

**One programme selected here is one run per apex**, provisioned in one transaction. The apexes that
already have an enumeration open are skipped rather than refusing the rest, so a perimeter whose second
domain failed gets that one going again while the first is still walking. Only a programme with nothing
free answers `run_in_flight`.

**The authorization window is checked here**, not just `state`. Without those two lines an expired program
would be provisioned on every tick and refused when the run opens: an execution billed to do nothing,
every thirty seconds.

**The absence of a run in flight is the condition that does the work.** It prevents a provisioning storm,
since `last_discovery_at` is written when the run is created rather than when it completes, and it bounds
concurrency. The bound itself is a unique partial index on `(program_id, apex)`: one live enumeration per
apex, which is the unit that has one, rather than per programme, which would let a perimeter of three
domains enumerate one of them.

`last_discovery_at` is written **at creation**, not at completion. A run that dies on the way must not be
restarted by the cadence while it is still in flight, and confusing the two would start two.

**The sweeper clears it when it expires a discovery run**, which is what actually provisions the
replacement. Left set, the condition above would hold a programme for a whole discovery interval after a
run that failed in thirty seconds: a week of coverage lost to a configuration error. Clearing it cannot
double-start, because by then there is no run in flight for the other condition to see.

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
