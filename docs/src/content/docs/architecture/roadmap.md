---
title: Roadmap
description: Nine phases in a constrained order, each closed by a milestone made of testable assertions.
sidebar:
  order: 16
---

:::caution[General rule]
The order is deliberately constrained. Each phase ends in a **verification milestone**: a list of testable
assertions. While one assertion fails, the phase is not finished and the next one does not start.

Never begin a phase whose predecessor's milestone is not green. The temptation to parallelize is the main
risk of this project.
:::

## Phase 0: Foundations

**Goal**: be able to write code without structural debt. The tooling choices are in the
[technical base](/architecture/stack/).

- [x] Repository initialized, [structure decided](/architecture/stack/#132-repository-structure)
- [x] [Migration tool wired](/architecture/stack/#134-migrations-goose), versioned and reversible
- [x] Local Docker Compose: [PostgreSQL](/architecture/stack/#133-self-hosted-postgresql), the Fingerprinter and its Chrome sidecar, on [the two networks](/architecture/verification/#85-network-isolation). FastRecon is not a service: it is an image a run starts
- [x] [Minimal CI](/architecture/stack/#138-tooling-and-ci): lint, tests, build
- [x] [Typed configuration](/architecture/stack/#136-configuration-and-secrets), no hard coded value
- [x] [Secrets out of the repository](/architecture/stack/#136-configuration-and-secrets), injected from the environment
- [x] [Database backup](/architecture/stack/#backups-the-consequence-of-self-hosting), archiving to a destination that is configuration, so the restore is exercised locally
- [x] [Two PostgreSQL roles](/architecture/deployment/#96-postgresql-roles), before any business migration

### Milestone 0

- [x] `docker compose up` starts everything in under 60 s on a clean machine
- [x] A migration can be applied then rolled back without loss
- [x] CI passes on an empty pull request
- [x] A backup is restored into an empty database from the configured archive destination, and the content is identical
- [x] Connected as `asm_app`: `CREATE TABLE` fails, `DROP TABLE` fails on a table the owner created, and reading and writing that table succeeds through the default privileges
- [x] From the fingerprinter's container, `psql` to the database fails, and the same connection succeeds from the control plane network

:::note[Measured]
**Cold start in 12.6 s**, from an empty volume with the images already pulled: PostgreSQL, one Chrome,
the rendering service, the migration, the role grant and the control plane.

**Reversibility** is proved by unwinding to version 0 and replaying, with a table the migration does not
own written in between. The roles disappear and come back; the row survives. Rolling back a schema that
carries no data would have proved nothing about loss.

**The restore is the assertion the archive destination change was made for**, and it is discriminating by
construction: the rows it checks are written **after** the base backup, so they exist only in the archived
WAL. Verified by removing the recovery signal, which brings the count back to one and fails with the
message that predicts it.

**The role confinement is checked in both directions.** Reading and writing an owner-created table must
succeed through the default privileges, otherwise every refusal beside it would pass just as well on a
role that can do nothing at all. `CREATE TABLE`, `DROP TABLE`, `ALTER TABLE` and `CREATE ROLE` are all
refused.

**Isolation reports what it proved rather than what one would like it to.** Between the docker networks
the rendering side reaches neither the database nor the internal API by name, and the control side reaches
the database, so the refusals mean isolation rather than a stack that is down. A published port is still
reachable from any container through the host gateway, because the local runtime proxies it to the host's
loopback. That is stated by the script instead of being claimed away: nothing in the deployed topology
publishes a port onto a host the rendering network shares.

**The environment guard is a linter rule, not a convention**, and it was verified by breaking it: a
`os.Getenv` outside the configuration package fails the lint with the reason attached.

**CI was ticked when the workflow ran**, not when its jobs passed locally. Those are two claims, and only
the second one was true for a while.
:::

## Phase 1: Data model and ingestion

**Goal**: freeze the [model](/architecture/data-model/). This is the most irreversible phase of the project.


- [x] `org`, `app_user`, `membership`, multi-tenant from the first migration
- [x] `program` and `scope_rule` with their validity windows and their `version`
- [x] `asset` with [canonical keys](/architecture/data-model/#43-canonical-keys), lineage and scope status
- [x] `observation`, **[partitioned monthly](/architecture/data-model/#partitioning-from-the-first-migration)** from the start
- [x] `asset_current` and `asset_layer`
- [x] `run` and `run_target`
- [x] `pivot_count`, `favicon_image`, and the seeded denylist with its idempotent seed
- [x] `org_id` on **every** business table
- [x] Indexes: GIN on JSONB and arrays, B-tree on promoted columns, the `reverse(key)` expression index
- [x] The single [`normalize(layer, data)`](/architecture/data-model/#normalization-comes-first) function, with its per layer schemas
- [x] `POST /reports` with [scope re-evaluation](/architecture/scope/#52-re-evaluated-at-ingestion) and [deduplication on write](/architecture/data-model/#deduplication-on-write)
- [x] [Geo-IP and ASN enrichment](/architecture/verification/#88-geo-ip-and-asn-enrichment) in memory in the control plane
- [x] [One authorization layer](/architecture/security/#111-irreversible-decisions) receiving a principal

### Milestone 1

- [x] Posting the same report twice creates **one** series of observations
- [x] Posting 1 000 identical observations creates **one** row, with `last_confirmed_at` moved
- [x] The measured deduplication rate exceeds 90 % on a replayed set
- [x] An out of scope asset is stored, never deleted, and marked `out_of_scope`
- [x] Changing a `scope_rule` reclassifies history without rescanning
- [x] A rule naming a host reclassifies its services and their URLs with it, and a `url_prefix` exclusion leaves the service carrying it in scope
- [x] A query carrying a different `org_id` returns **no** row
- [x] A report attached to an expired `program` is rejected at ingestion
- [x] A report naming a host outside its run's frozen target list is **rejected**, not ignored
- [x] Monthly partitions are created automatically, and a row outside any partition fails
- [x] Connected as `asm_app`: `DROP TABLE asset` fails, writing to the seeded denylist fails, reading it succeeds, and `INSERT INTO asset` succeeds
- [x] The ingestion cost stays under the round trip budget per observation, measured rather than estimated

:::note[Measured]
**Milestone 1 is closed.** The two assertions that took longest were the ones nobody can tick by reading:
the round trip budget needed a tracer, and "partitions are created automatically" needed something to
call the function without being asked.

**1.50 round trips per observation**, counted by a pgx tracer rather than estimated, against a budget of
three. One report of one host writes two assets and four observations in six statements: the identity and
its projection travel in one, and so do the deduplication lookup and whatever it decides.

**Deduplication rate 0.950** over 4 000 submitted observations. Twenty passes rather than ten, because the
first can structurally deduplicate nothing: at N passes the arithmetic ceiling is (N-1)/N, and ten would
put that ceiling exactly on the threshold.

**A thousand identical reports leave one row per layer**, with the confirmation window widened rather than
the row rewritten. That is what "this state held from here to there" has to mean for the timeline to be a
list of changes rather than of probes.
:::

## Phase 2: Verification and lifecycle

**Goal**: liveness on a direct target. Usable immediately on a hand entered list, which is what makes this
phase testable before discovery exists.

- [x] [Run definitions](/architecture/deployment/#91-the-run-contract), signed target URL, signed report token
- [x] [Due date selection](/architecture/lifecycle/#63-scheduling-and-backoff) and the frozen target list as the lease
- [x] Deadline sweeper for runs, and the console refusal that names the run
- [x] [Backoff with jitter](/architecture/lifecycle/#backoff-curves), two curves
- [x] [Failure qualification](/architecture/lifecycle/#where-the-qualification-is-carried), informative against non informative
- [x] [The `lifecycle` state machine](/architecture/lifecycle/#62-state-machine), excluding `unobservable`, which depends on phase 3
- [x] [Projection onto `asset_current`](/architecture/lifecycle/#three-layers-one-lifecycle): layer states, promoted columns, reachability counters
- [x] [An open port becomes an asset](/architecture/verification/#an-open-port-becomes-an-asset), with its bound
- [x] [Dangling CNAME detection](/architecture/verification/#takeover-candidates) into a structured field
- [x] [Dead origin signatures behind a CDN](/architecture/verification/#dead-origin-behind-a-cdn) and `is_cdn` detection
- [x] Manual asset entry, under a scope action rather than the ingestion one: a host due for `full`, a URL scheduling the service it belongs to

### Milestone 2

- [x] A hand entered list of 100 FQDNs produces a correct inventory
- [x] A hand entered host produces its open ports and their services on its **first** run, rather than a resolution alone
- [x] A hand entered URL earns no render until its service has answered, and is then **scheduled** at the path as declared
- [x] An `nxdomain` confirmed 3 times over more than 24 h moves the asset to `INACTIVE`
- [x] An `nxdomain` confirmed 3 times in 90 minutes does **not**
- [x] A repeated timeout **never** produces an `INACTIVE`
- [x] An asset that comes back after a failure returns to `ACTIVE` on a single success
- [x] A host absent from a truncated report gets no observation and keeps its due date
- [x] A run killed mid flight blocks nothing: its targets are selected again on the next tick
- [x] Two concurrent runs never hold the same host
- [x] A port found open becomes a `service` asset, and 25 open ports on one host derive nothing
- [x] A dangling CNAME is recorded with `kind`, target, signature and timestamp, which is enough for phase 5 without recollecting anything

:::note[Measured]
**Milestone 2 is closed**, and one assertion was corrected rather than met. It read "and is then
**rendered** at the path as declared", which asks for the renderer, and the renderer is phase 3. What
phase 2 owns is the schedule, so the assertion now says scheduled, and the render at the declared path
is asserted where the render exists. That is the drafting error rule, not a relaxation.

**The state machine is asserted on a clock the test owns.** Every threshold here is a shape rather than
a count, "three failures over at least twenty four hours", and a suite that lets real time pass can
only assert the count. Three nxdomains inside ninety minutes is the discriminating case, and it does
not exist without an injected clock: removing the floor leaves the suite green in every other test and
fails exactly that one.

**2.33 round trips per observation**, up from 1.50 and against the same budget of three. The layer
counters, the lifecycle and the promoted columns travel in the statement that writes the observation,
so the transitions cost nothing extra; what the difference buys is the rescheduling of each host and one
sweep per report for declared URLs.

**The `tcp` layer speaks.** The sweep counts landed in the scanner at schema 1.1 while this phase was
being closed, so the layer that was written to stay silent without them now concludes on its own
evidence ([8.1](/architecture/verification/#what-the-port-sweep-can-conclude)). Nothing changed here to
make that happen, which was the point of writing it that way, but the transcription was wrong in one
place: `degraded` sits **inside** `run` and this read it from the top level of the document. It compiled,
and every test that built a report in Go passed, because the position of a field only exists once a real
document is decoded. There is now one test that decodes one, and removing the fix fails it.

**Every fix was checked by removing it**: the lease exclusion, the silence of a truncated run, the
downgrade of a degraded one, the filtered-port guard, the persistence of the backoff tier, and the three
result threshold on reachability. Each leaves the rest of the suite green and fails only its own
assertion, which is the whole of what rule 8 asks.

**A review after the milestone found eight defects, all of them real, and two of them silent and total.**
A discovered host never received a `full` due date, because the list of scopes that move it named the
ones that sweep and missed the one every discovery run carries, so nothing would ever have swept a
discovered host's ports a second time: a port opened next week was invisible, which is the single thing
scanning exists for. And an archived asset could never come back, by hand or on rediscovery, while the
endpoint answered that it had been scheduled. Both were ticked boxes with a passing suite, and the
harness hid the first by leaving a run's scope empty.

The other six: one live verification run per programme was a check rather than a fact, so two
overlapping ticks froze the same hosts; Cloudflare 522 was named in a comment and missing from the guard
beside it; a `tcp` observation claimed it could clear a takeover finding only `dns` can produce; a
resolution that timed out cleared the CDN flag on a fronted asset; the backoff curve was applied
unchanged to the expensive rung; and a sweep reporting open ports could still be read as a host with
nothing listening.

**Two boxes were ticked before anything asserted them, and writing the assertions found a bug.** The
backoff tier and the reachability counters were implemented, unit tested where the arithmetic lives, and
never checked through the database. The tier round trip had an off by one: it was incremented before the
delay was read, so **every curve started at its second rung**. On the flapping curve that is an hour
instead of fifteen minutes; on the candidate curve it is five minutes instead of one, and that first
minute is the whole of the freshness advantage. A unit test on the curve could not see it, because the
curve was right and the caller was not.
:::

## Phase 3: Fingerprinting

**Goal**: [baseline, reachability, and death behind a CDN](/architecture/verification/#82-the-fingerprinter).
Inseparable from phase 2 in use.

- [x] The service deployed on an [isolated network](/architecture/verification/#85-network-isolation), two Docker networks locally
- [x] No route to PostgreSQL and none to the internal API, **verified rather than assumed**, with a positive control
- [x] The service holds **no credential**
- [ ] SSRF guards: internal ranges refused before the request and at every redirect hop
- [x] `POST /scan` from the control plane, with a render costing more than one against the budget
- [x] `high` and `low` queues, a baseline entering low
- [x] [The five triggers](/architecture/verification/#83-when-a-render-happens) and the baseline filter
- [x] [Reachability per observer](/architecture/verification/#86-reachability-per-observer), signed counters, the four regimes
- [x] The [`unobservable`](/architecture/lifecycle/#64-qualifying-a-failure) state and the per program threshold alert
- [x] [`producer_version` and `last_producer_version`](/architecture/verification/#87-dating-the-instrument)
- [x] Periodic cadence **per regime**, four values rather than one ([corrected](#the-two-red-lines-of-phase-3))

### Milestone 3

- [x] From the service's container, `psql` to the database **fails**, and the same connection **succeeds** from the control plane network
- [ ] A request toward `169.254.169.254` is refused, and so is a redirect hop toward an internal range
- [x] An asset whose two probes fail 3 times becomes `unobservable`, **not** `INACTIVE`
- [x] An asset at 403 on the HTTP layer and 200 on the render flips to the protected regime
- [x] A dead origin behind a CDN is detected as dead, including when the render fails at the same moment
- [x] No screenshot is present in PostgreSQL
- [x] A render that produces no chain writes an observation, and the asset's counters move
- [x] `last_fingerprint_at` does not move on a render that obtained no page
- [x] A pass over 500 assets does not exceed the program's rate limit

### The two red lines of phase 3

:::caution[Phase 3 is not closed, and phase 4 does not start]
**The SSRF guard is not implemented, and it is not implemented here.** Measured against the running
image on 21 August 2026, the rendering service navigates to `169.254.169.254`, to `127.0.0.1` and to
`10.0.0.0/8`: the errors it returns are Chrome reporting what happened *after* it sent the request. On a
host where the metadata service actually answers, the same call returns instance credentials. A headless
browser that renders attacker controlled pages and will follow a URL to a link local address is the worst
thing this component can be, and the second half, re-checking at every redirect hop, is the one that
actually gets exploited.

The control plane refuses to submit such a target, so the guard exists on the caller and is exercised by
two processes that fail differently. That does not close the line: a check on the caller is a convention
where a check on the service is a property, and the service is not only ever called by this. **The
request has been written and transmitted.**

**The cadence item was corrected rather than met.** It read "periodic cadence modulated by volatility",
and this document's own post-v1 list defers volatility with an argument this phase does not overturn: the
tiers need weeks of real data, and fixing them on a few hundred assets produces invented thresholds that
later read as measurements. What phase 3 owes is a cadence per **regime**, which is the table of
[8.6](/architecture/verification/#86-reachability-per-observer) and is four values rather than one. That
is built and asserted. Volatility stays where it was already written down.
:::

:::note[Measured]
**The render path holds no state between two ticks**, and that is what makes it need no recovery
mechanism. The queue is a predicate re-evaluated every pass, a render has no lease, and the due date is
the queue. A saturated service is asserted to touch nothing at all: no observation, no counter, no
timestamp, and no due date moved, so everything it refused is still due on the next tick by the same
ordering that put it there.

**The order of two lines turned out to be the rule.** The observer's counter has to move *before* the
state is decided. A pass that decided first entered `unobservable` one observation late and left it one
late too, which is two rounds on a threshold of three, and it is exactly the mistake
[6.4](/architecture/lifecycle/#64-qualifying-a-failure) warns about when it says leaving that state reads
the current observation rather than the column.

**A first contact is not a change.** Trigger 2 fires on a diff the HTTP layer detected, and wiring it to
"an observation was inserted" put every service of a fresh perimeter into the `high` queue, which exists
to stay short. It fires only where there was a previous state to differ from.

**A baseline is armed once**, by a statement that applies only where there is no render date. Routing it
through the promote path left it at whatever the column defaults to, which is the urgent priority: the
low queue would have been empty and the high queue would have held the whole inventory.

**Verified end to end against the real service**, not a fake: one due service, one pass, one page
obtained, the observation written with its chain and its producer version, no screenshot anywhere in the
database, and the next render three weeks out.

**Six boxes were ticked before anything asserted them**, and the habit from phase 2 held: the five
triggers had one tested out of five, the regime cadences were asserted on the arithmetic and never
through the database, and the mass tip alert existed with nothing reading it. Writing those assertions
found no new bug, which is a result rather than an anticlimax: it is the first time in this project that
the audit came back empty, and it only means something because the previous two did not.

**One of those checks proved nothing the first time.** Reverting the port filter left the package
failing to compile, and the pattern matching the test output hid the build error, so the run looked
green. A revert that does not compile asserts nothing at all, and the fix is to read the whole output
rather than the lines a filter expects.

**A second review found eight defects, all real, and two of them made the render loop useless in
different ways.** Starvation was ending a pass rather than a programme: workers on the same one all
compute the same wait, all wake together, and exactly one wins the retry, so a deployment holding one
programme rendered two assets a tick whatever its batch size and every other programme's work was
dropped with it. And only a *successful* render moved a due date, so an asset a browser can never be
pointed at was re-selected every minute forever, at the head of the queue, until enough of them stopped
the pass reaching anything renderable at all.

The other six: a udp service earned a browser baseline, and the render queue carries no protocol to
notice with later; the scheme was stored as the report wrote it, so `HTTPS` on port 8443 dropped the
port from the authority and pointed a render at a different service on the same host; the promote path
had no opinion about ports a browser refuses where the baseline path did; `queued` was read from the
lifecycle alone while the statement also requires the perimeter; a replan spread under a second divided
by zero; and the guard knew only the canonical address spelling, so `http://2130706433/` and
`http://0177.0.0.1/` walked past it to a resolver that fails on them and a check that reads a failed
resolution as somebody else's problem.

**The unobservable census is throttled**, at five minutes rather than at the pass. It groups over the
whole projection and the number it produces only moves when observations do, so taking it on every tick
would buy nothing for a full scan a minute.

**The local topology cannot express "calls but is not called by".** Joining the control plane to the
scan network would let the browser side resolve `postgres` and `controlplane` by name, so the call goes
out through the host gateway instead, which is the path the isolation script already names rather than
claims away. Both directions still check out: the scan side reaches neither the database nor the
internal API by name, and the control side reaches the database.
:::

## Phase 4: Discovery

**Goal**: [FastRecon over a program's apexes](/architecture/discovery/), feeding the loop of phase 2.

- [x] [Run definition generated from `scope_rule`](/architecture/scope/#51-the-scope-belongs-to-the-control-plane), apexes and exclusion patterns
- [x] [Source credentials](/architecture/discovery/#73-source-credentials) named and never carried, so a start call cannot wipe them
- [x] [The curated port list](/architecture/discovery/#74-the-port-list) travelling in the run definition
- [x] [Scheduled runs](/architecture/deployment/#98-starting-runs) per program and the console endpoint
- [x] Assets entering through the normal path: ingestion, scope, due dates
- [x] [Per host source attribution](/architecture/data-model/#44-lineage) feeding lineage

### Milestone 4

- [x] A run killed midway keeps everything already delivered
- [x] No out of scope asset receives a due date
- [x] Rescanning the same perimeter twice creates **no** observation row on unchanged assets
- [x] A run that never completes is marked expired and a replacement is provisioned
- [x] [`discovery_path`](/architecture/data-model/#44-lineage) is populated and usable on every asset
- [x] A source without a key reports itself, and the run says in one line what it could query

:::note[Measured]
**The loop closed against the real platform**, which is where two contract errors surfaced that nothing
in this repository could have caught. `--stages` takes a **scope name** and not a list of stages, so the
first real run failed on its configuration one second after starting: both sides of that invention lived
here, so the suite was green. And a start call **replaces** a definition's arguments while **merging**
its environment, so a flag on the definition beats a variable sent at start, and the deployed one carried
`-d hackerone.com`. Every run would have scanned that, and nothing would have looked wrong.

**The run said in one line why it found nothing**, which is the assertion above working rather than a
disappointing result: `1 of 2 sources answered`, with the failing one and its status code stored beside
the report. Without that accounting an empty inventory and a broken source are the same screen.

**Two boxes were red for a reason a test found rather than a review.** A dead discovery run held its
programme for a whole discovery interval, because `last_discovery_at` is written at creation and nothing
cleared it: a run that failed in thirty seconds cost a week of coverage. And a derived service copied its
host's lineage, so "what did this source find" answered with every service in the inventory rather than
with the names the source returned.

**A review afterwards found six defects, and the sharpest was the one this phase had just taken trouble
over.** The default runner logged the whole invocation at INFO, and the invocation carries two live
bearer tokens: one that posts the run's report and one that fetches its frozen target list. The same
diff had just moved a credential out of a URL because "a token in a URL is a token in every access log",
and then wrote both of them into the log directly. Invocations go through a redaction that lives beside
what builds them, so a call site cannot forget it.

The others: the post-commit start ran on the request context, so a console hanging up mid-call could
cancel a start the platform had already accepted or lose the name it gave the execution; clearing
`last_discovery_at` to schedule a retry destroyed the record of when a programme was last enumerated and
turned a permanently broken runner into a loop that provisions and bills on every tick, so the retry is a
delay in the due query instead; a programme with no apex was selected and refused once a minute forever;
an exclusion the scanner cannot be given was dropped in silence rather than named; and the target list
endpoint accepted a credential without its `Bearer` scheme where the report endpoint refuses one.

**Two findings were checked and did not hold.** The start response envelope was called wrong against
`v1alpha1`, and the deployed API is `v1alpha2`, which returns the list this parses, as the live run
proves. And `--stages full` on a verification run was called an unbudgeted enumeration, where the
scanner's own documentation says a supplied list replaces enumeration rather than skipping it. The
second half of the first finding was fair and is fixed: there was no test over the start call at all, so
a mismatched envelope would have been a warning and an execution nobody could find the logs of.

**One assertion is ticked for the half this repository owns.** A source with no key reporting itself is
FastRecon's, and it is proven live. An **empty** variable being an error rather than "no key" is also
FastRecon's, and it was not verified here. What Recon owes is that it names credentials and never carries
them, which is asserted on the definition it produces.
:::

## Phase 5: Diff and notifications

**Goal**: [the system becomes better than a script](/architecture/notifications/).

- [x] [`notification_event`](/architecture/notifications/#123-where-the-event-is-produced) written **inside the ingestion transaction**, payload frozen, with its `CHECK`
- [x] Monthly partitioning, and a maintenance loop that starts unconditionally, for **both** partitioned tables
- [x] [Asymmetric retention](/architecture/notifications/#127-volume-and-retention)
- [x] The write folded into the statements that already exist
- [x] [Structured diffs](/architecture/notifications/#121-structured-diffs) per field type
- [x] [Revelation against real change](/architecture/verification/#87-dating-the-instrument) classification
- [x] The [event table](/architecture/notifications/#122-events-worth-notifying), excluding `geo_anomaly` and `external_host_dead`
- [x] Notifier loop, and the two program events it alone can produce
- [x] [Anti-flood](/architecture/notifications/#124-aggregation-and-anti-flood) and the [first run grace](/architecture/notifications/#125-the-first-run-of-a-program) with its age guardrail
- [x] [Per organization channels](/architecture/notifications/#channels-belong-to-an-organization) and a generic templated webhook

### Milestone 5

- [x] An application version bump produces a readable notification saying *what* changed
- [x] A rescan with no real change produces **no** notification
- [x] A first run produces **one** summary and its whole flood suppressed, on a real perimeter rather than a synthetic one
- [x] On the same set, a later run turning 5 000 assets active sends at most 20 notifications per window, the rest carried by summaries, and **no event lost**
- [x] A takeover candidate is notified at critical priority, at most one Notifier tick after the event was written
- [x] A fingerprinter version bump produces diffs classified as detection improved, with no alert
- [x] A dangling CNAME recorded in phase 2 produces a critical notification **without recollecting anything**
- [x] A mass tip into `unobservable` triggers a distinct alert rather than being swallowed by a summary
- [x] A webhook answering 500 marks **no** event notified, and sending resumes on recovery
- [x] A program event with no `asset_id` is accepted, and a `takeover_candidate` without one is **refused by the database**
- [x] A failed first run leaves the grace active; a later successful one ends it
- [x] A grace nothing ends expires at 7 days, emits its summary and reports the incident
- [x] The ingestion cost stays under budget, events included, on a batch where everything changes



:::note[Measured]
**Three bugs were found by writing the assertions, and the second is the one that mattered most.**

`new_active` fired once per **observation** rather than once per asset, so one host arriving notified
twice and a host with a service three times, and every one of those lines was true. A transition happens
once per report and is told once.

**A transition on a deduplicated observation produced nothing at all.** The diff needs a row to compare
against, so the write path returned early where none was written, and the transition rode along in that
early return. A death is three identical `nxdomain` answers and the transition lands on the **third**:
the most common death in the system was silent. What the asset became is now told whether or not a row
was written; what changed in the world is still told only where there is something to compare.

**The comparison ran on the payload as it arrived against the stored one, which is normalized.** So every
field normalization touches read as a change forever: a version it drops, a cookie map it turns into
names. It also broke the revelation classification, because a pure addition arrived mixed with two
phantom ones, so a detection improvement alerted as a real change. Both sides are normalized now, by the
same function, which is what [12.1](/architecture/notifications/#121-structured-diffs) asks for in the
sentence about two divergent implementations.

**The same silence existed twice, and the second one only appeared at volume.** Past the window cap the
individual events were suppressed and no summary followed them either, so a run turning five thousand
assets active sent twenty notifications and swallowed the rest without a word. A saturated window now
writes one summary, once per window, carrying **the priority of the window it replaces** rather than its
own: a flood of high events summarised at medium would be refused by a channel whose floor is high, which
is the same loss by another route.

**A first run held back its whole flood and said nothing at all**, which is the anti-flood becoming the
silence it exists to prevent. On a real perimeter, forty eight arrivals were suppressed correctly and no
summary followed them, because nothing emitted one. The summary is written when the grace ends, once per
programme, as a programme event so it escapes the windows, and it names the count it stands for.

**The channel bootstrap only ran at startup.** The organization is created by a command outside this
process, so a deployment that configured a webhook and then created its tenant had no channel until
somebody restarted for an unrelated reason. The notifier retries until there is a tenant to attach to.

**A review afterwards found fourteen defects, and the three serious ones were each a notification
saying the wrong thing rather than a crash.**

A dangling CNAME re-alerted on **every pass**. The finding is re-derived from every report and critical
escapes the windows, so telling it from the transition path meant the same alert on every scan cycle,
forever, for every dangling asset in an inventory. It rides the insert path now: a re-confirmed finding
deduplicates and says nothing.

The overflow summary was written on the **first** event past the cap and counted what was held **at that
instant**, which is one. So a flood of five thousand produced a summary claiming to speak for one event
while four thousand nine hundred and eighty went unmentioned, and the assertion passed because it only
checked that a summary existed. Summaries are written once the tick is over, counting what they actually
stand for.

And any digest excluded a programme from its onboarding summary, permanently. A large first run
saturates the high window on port openings, which the grace does not hold, so the overflow digest landed
first and the onboarding one never: the whole point of the commit before it, undone by a query that did
not distinguish the two kinds of summary.

**The revelation classification was running on every layer.** The scanner's version is stamped on the
dns, tcp and http observations too, so for one pass after every scanner upgrade a newly opened port was
reclassified as a low priority "detection improved" across a whole inventory. It applies to the layer
the classification is about.

**The round trip budget went to 3.00 and came back to 2.67.** Reading the first run grace in a query of
its own cost one round trip per report, and the statement that already stands between the credential and
the write had the three facts in reach.
:::

## Phase 6: Search and facets

- [x] [Pivot promotion into `asset_current`](/architecture/search/#102-what-the-projection-carries), the precondition for everything else
- [x] [Structured AST to parameterized SQL](/architecture/search/#101-three-principles), with a field registry
- [x] [Facets over the filtered result](/architecture/search/#104-facets-are-the-real-cost)
- [x] [`pivot_count` maintained on write](/architecture/search/#105-pivots): increment, value loss **and** archiving
- [x] [Genericity filter](/architecture/search/#the-genericity-filter) applied to display only
- [x] [`favicon_image`](/architecture/search/#the-favicon-image-is-not-in-attributes) and its size bound
- [x] [Volatility](/architecture/search/#106-volatility-as-sliding-daily-buckets) as sliding buckets
- [x] Stable pagination and [export](/architecture/search/#108-export)
- [x] [`external_host_dead`](/architecture/notifications/#what-external_host_dead-can-actually-see), both halves
- [x] [The static tenant guard](/architecture/security/#111-irreversible-decisions), which is what inventories the cross-tenant queries the third role has to cover
- [x] [The third role and the second pool](/architecture/deployment/#96-postgresql-roles), and [Row-Level Security](/architecture/security/#row-level-security-two-roles-rather-than-one-variable) enabled

### Milestone 6

- [x] The RLS suite **refuses to run** if `current_user` is a superuser, carries `BYPASSRLS`, or owns the tables
- [x] An `asm_app` session set to one organization reads **no** row of another, without the query carrying `org_id`
- [x] A connection acquired **without** the session variable returns zero rows, never the whole inventory
- [x] The cross-tenant queries cross tenants under `asm_sys` and return only the set organization under `asm_app`
- [x] A four clause query on a **synthetic** set of 1 M assets, with a realistic distribution, answers in under 300 ms
- [x] Facets reflect the filtered result, not the global inventory
- [x] A pivot with a counter of 1 is not displayed, and a universal cookie name never appears as a pivot
- [x] An asset losing a pivot value decrements its counter; an archived asset gives back **all** of its own and loses the keys
- [x] A suffix containing a wildcard character still matches by suffix rather than by equality
- [x] The export applies no display filter and imposes no silent cap
- [x] The ingestion cost stays under budget, pivots included, measured on a batch that moves some
- [x] **No query touches `observation`**, proved by revoking the privilege

:::note[Measured]
**Two things were added that the phase did not list, and both were preconditions rather than scope creep.**

The **static tenant guard** was described in [11.1](/architecture/security/#111-irreversible-decisions) and
implemented by no phase. Its output is the inventory of cross-tenant queries, which is the exact
specification of what `asm_sys` has to cover, so the second pool could not be routed without it. It found
nothing wrong on the day it landed and it has already earned itself since: adding the external host sweep
made the list grow, and the build said so before the pool routing could be forgotten.

The vocabulary grew a fourth value, `keyed`. `UPDATE asset_current ... WHERE asset_id = $1` names no
organization and is not crossing tenants either, and calling it `scoped` would have put it in the column
that says "this one carries its own filter", making the audit list useless. Most statements in the
repository take that shape.

**`COPY FROM` is refused outright on a table carrying a policy.** Two writes were built on the copy path
for the round trip budget, freezing a run's target list and writing a report's events, and both stopped
working the moment isolation was enabled. They travel as parallel arrays now, same single round trip, and
the organization became a scalar of the statement rather than a column repeated per row, so a batch mixing
two tenants is no longer expressible.

**A partition reached directly is not covered by its parent's policy.** `SELECT count(*) FROM
observation_2026_08` as `asm_app` returned every tenant's rows while the same count through `observation`
returned one tenant's. Nothing in this repository names a partition, which is exactly the sort of statement
row-level security is the last line for. The door that creates partitions is now the door that covers them.

**An unset session variable is an empty string, not a null**, and the difference decides between zero rows
and an error. A policy reading `current_setting(...)::uuid` returns zero rows on a connection's *first*
transaction and raises on its second, so a suite acquiring a fresh connection per case passes with the
fault in place. The assertion reuses one connection across two transactions.

**`last_changed_at` was set on a first observation**, which contradicts what
[10.6](/architecture/search/#106-volatility-as-sliding-daily-buckets) says it means, and nothing read the
column so nothing noticed. Volatility inherited the same test: an arrival is the asset's age and the row
already carries it, and counting it here would count it once per layer, so a freshly discovered asset would
score three or four and `volatility > 2` would return everything just found.

**The first volatility assertion did not discriminate.** The guard sits on two columns, the buckets and the
day they belong to, and a sum read against a day nobody wrote is zero whatever the buckets hold: removing
half the guard left the test green. It asserts the array as well as the number.

**`status_chain` was a column no producer could fill.** The scanner reports the redirect **URLs** and the
final code, never the code of each hop, so the layer the doc assigned it to does not hold the information.
The browser reports one hop per entry with its status, so the render writes it, and the coverage that costs
is written down rather than discovered.

**The suffix on `key` cannot answer "everything under this domain".** A service is keyed
`app.target.com:443/tcp`, so `.target.com` is a suffix of the name and not of the key: the query returns the
fqdn rows and silently drops every service, which is most of an inventory. `host` carries the reversed index
too now.

**A decrement cannot travel through an upsert.** PostgreSQL evaluates a table's CHECK constraints on the
proposed row *before* it resolves the conflict, so a proposed count of `-1` fails `count >= 0` even though
the row it would become is an update to zero. One error message away from somebody removing the constraint,
and the constraint is what catches a decrement running twice.

**Nothing that can carry a pivot can currently reach `archived`.** The transition is decided when a
candidate **host** exhausts its budget without ever coming alive, and such a host has no service, no render
and therefore no pivot. The decrement is built anyway, because retrofitting a counter's decrement means
first working out how far it has drifted, and it is tested against the statement the system uses rather
than against a case the model cannot produce.

**Technologies is the one place "one producer per value" bends**, and it bends because the two arguments
used elsewhere point in opposite directions: coverage says the probe must contribute, depth says the render
must. They write different keys and the column is their union, so neither erases the other. The first test
of it did not discriminate, because both producers happened to report `nginx`.

**The round trip budget reads 3.00 and it did before this phase too.** Nothing here added a statement to
the write path; what changed is that the counter can now see one it never could. pgx traces a `COPY`
through a different hook, so the two writes that moved onto the copy path in phase 5 became invisible to
the measurement and the figure fell by a third without the work changing. The gap is written into the
counter rather than left to be rediscovered.

**The four clause query over a million synthetic assets answers in 164 ms**, and its facets in 414 ms. That
is the number the whole chapter rests on: it is what says a double write to Elasticsearch on day one would
have been a trap rather than foresight.

**A review afterwards found nine defects, and the three that mattered were each a correct-looking answer
rather than a failure.**

`technologies` stopped being written by a layer and started being derived from two keys of `attributes`,
and neither key exists on a row written before the migration. A render landing on such a row computed the
union from its own key alone, so everything the probe had ever reported disappeared from the column until
the next full HTTP pass, which can be a week away. The migration carries the column into the key, which is
exact: before this phase nothing but the http layer wrote it.

**A failed DNS lookup erased a finding rather than leaving it alone.** The sweep rewrites the list of dead
external hosts wholesale, and an apex it could not resolve simply fell out of it, so the next tick that did
resolve read the same host as newly dead and re-sent a critical alert about a domain that had been gone all
week. One test asserted that a failure creates nothing; none asserted that it destroys nothing.

**The job that creates next month's partition re-applied the policies to every table**, five `ACCESS
EXCLUSIVE` locks each, held until it committed. The first tick of each month would have blocked the whole
application on `asset_current`, and since the partition set only grows, the same tick would eventually hold
several hundred locks and fail on `max_locks_per_transaction`.

Six more, each small and each real. The sweep capped the tick rather than the page, so a deployment with
more references than the cap left its tail permanently unswept and said nothing. It rewrote every asset it
looked at, including the ones whose verdict had not moved. The export sent `200` before its first query
ran, so a database refusing it arrived as an empty file. A nested empty group compiled to "no constraint",
which under a negation answers the whole inventory where the honest answer is nothing. The second pool
read the first one's connection bound, so a deployment tuned for one budget opened two. And readiness
probed only the application pool, while the system pool is what every authenticated request needs first.

One of the nine has no assertion behind it and it is worth saying which: the readiness probe. Testing it
means standing up the whole route set against two pools to observe one status code, and the change is two
lines of wiring. It was verified by reading.
:::

## Phase 7: Console

- [x] [`recon bootstrap`](/architecture/deployment/#97-bootstrap), a precondition for the rest of the phase
- [x] [SvelteKit console](/architecture/console/) with no database credential, and the token exchange
- [x] [Host grouped list](/architecture/search/#107-the-list-is-a-list-of-hosts) with facets
- [x] [Distinct timestamp on fingerprinter badges](/architecture/search/#what-a-row-carries)
- [x] [Three enrichment states](/architecture/search/#three-states-of-enrichment), the console holding that state from the server
- [x] [Takeover candidate rendered on the row](/architecture/search/#what-a-row-carries)
- [x] [Asset view with its timeline](/architecture/search/#109-the-asset-view), the project's first read of the journal
- [x] [Live SSE feed](/architecture/search/#1010-the-live-feed-of-discoveries), polled on a cursor
- [x] [Program and scope management](/architecture/scope/#55-managing-the-perimeter), reclassifying in the same transaction
- [x] [Queue view](/architecture/deployment/#99-reading-the-queue)

### Milestone 7

- [x] Deciding "open it or move on" on a row in under a second
- [x] No composite score, no severity, no environment label, held by a test on the **contract** rather than on a screen
- [x] An `unobservable` asset is visually distinct from an `INACTIVE` one
- [x] Geolocation is not shown on a CDN asset
- [x] Every pivot badge is clickable and re-runs a search
- [x] A row with no cookie badge distinguishes the three causes
- [x] On a deployment with no Geo-IP database, the infrastructure family is **not shown** rather than empty
- [x] A row carries **no** script hash badge, and the same hash stays searchable and counted
- [x] `recon bootstrap` creates a usable organization without a line of SQL, and replayed it creates no second one
- [x] A service discovered and never probed is found by a filter on the port its key carries
- [x] The live feed re-emits nothing on an unchanged rescan, and says what a cap left out
- [x] A scope rule write grants or removes due dates in the same transaction, and a failed reclassification writes no rule
- [x] A write carrying a stale version answers 409 and applies **nothing**
- [x] The console **refuses to start** without `ORIGIN` outside development; in a production build, a cross-origin POST and a POST with no `Origin` both answer 403 where the same form in same origin answers 200

:::note[Measured]
**The vocabulary grew one field, and it is the one that looks like the tenant.** The switcher sits on every
screen and is exactly a filter on `program_id`, which phase 6 had refused alongside `org_id` as though the two
were the same kind of thing. They are not. The organization is emitted by the compiler on every compilation,
so a query can name it in neither direction; a program is a perimeter inside one organization, and the tenant
clause is still emitted beside it, so naming somebody else's program returns nothing rather than their
inventory. The test that refused both now asserts both halves.

**The list is a second route rather than a flag**, because the two cursors mean different columns. A grouped
page walks `(max(last_seen), host)` and a flat one walks `(last_seen, asset_id)`, so behind one route a client
that flipped the flag and kept the cursor would get a walk that restarts or skips. Behind two, and with a
prefix on the feed's, every mix-up is a refusal: none of the three decodes as another.

**The group cursor bounds the group and never the row.** Bounding the rows as well reads like a free
narrowing and is wrong: dropping the rows above the cursor changes what `max()` is computed from, so a host
already returned comes back with a smaller maximum and passes the bound a second time. The cost of not doing
it is a grouping over the whole filtered set on every page, which is stated rather than hidden.

**The bootstrap is idempotent on the email and not on the organization name**, which is the correction the
doc pass made before the code did. `app_user.email` carries a UNIQUE constraint and `org.name` deliberately
does not, since two customers may legitimately be called the same thing, so keying on the name would have
been this command hoping where the database enforces nothing.

**The feed emits one message per round and not one per discovery.** One per asset would put the cap in the
wrong place: a round that found four hundred would emit four hundred messages of which the client keeps the
last fifty, and the overflow the round exists to announce would have no message to travel in. A round that
finds nothing emits nothing at all, because an empty event per tick would advance the id on every tick and a
client resuming from it would be resuming from a position that never named a discovery.

**`first_seen` needed an index nothing else in the system wanted.** The list orders on `last_seen` and the
due date passes order on their own columns, so the feed was a scan of the tenant per tick until phase 7 added
one.

**The timeline reads one row past its cap and never shows it.** Without that row the oldest displayed entry
of a cut layer would say "not compared" while the state before it sits one row away, inside the window. The
cap is reported and the window never is, because the two absences are not the same: one is a fact about the
asset, the other is this view announcing its own settings.

**The console is a second toolchain and it has a job of its own in CI.** Nothing generates the TypeScript
shapes from the Go ones, so a field renamed on one side arrives on the other as `undefined` and draws an
empty badge rather than failing. The type check is the only thing standing between the two, and it caught the
whole contract move: `asset_id`, the pivots carrying the server's badge verdict, the lineage becoming objects
instead of strings, and the layer verdicts speaking `asset_layer`'s vocabulary.

**The row's sentence needed three columns the projection did not carry.** "The name no longer resolves" and
"every probe failed" are two different findings and the lifecycle can write neither, so `dns_state`,
`tcp_state` and `http_state` were promoted into what the list reads.

**Two assertions did not discriminate on the first try, and both were found by breaking the code.** The
fixture behind "an excluded asset loses its due dates" set one of the three columns, so an exclusion that
cleared only `next_resolve_at` passed while two thirds of the guard proved nothing. And the badge test that
was meant to prove a denylisted cookie is not drawn passed for the wrong reason: it carried the pivot without
the attribute the value came from, so the row said "sets no cookie" where the honest answer is "sets one no
badge deserves".

**The stream and the page cannot share a path.** A `+server.ts` beside a `+page.svelte` is resolved by the
Accept header, so the same URL answers a page to a browser and a stream to anything else. It worked in a
browser and hung on the first client that accepted anything, which is how it was noticed.

**`run.summary` became a wire contract, so its fields are tagged.** The queue view reads that document back,
and without tags the keys were Go identifiers: renaming a field would have changed what a console reads with
nothing failing to compile.

**One route was deleted rather than ported.** The old console fetched a fold of raw evidence per row; the row
that replaced the card does not have one, so the endpoint had no caller. A surface nobody calls is a surface
somebody keeps working for nothing.
:::

## Phase 8: Certificate Transparency

**Goal**: [the freshness advantage](/architecture/vision/) becomes real.

**It comes last, and that is a decision rather than a leftover.** Nothing in the phases before it consumes
Certificate Transparency: its output is candidate assets, which the verification loop already knows how to
handle. Everything else it would need, the aggressive backoff and the single host run, is built by then. So
it can move without dragging anything, which is exactly what makes it the safe thing to put after the
console.

What the position costs is worth writing rather than discovered later: CT is what the vision calls the
freshness advantage, so it is the differentiator. Deferring it blocks nothing and delays precisely that.

- [ ] [`certstream-server-go`](/architecture/discovery/#75-certificate-transparency) deployed
- [ ] [Matching by label walk in an in-memory set](/architecture/discovery/#75-certificate-transparency), **no regex**
- [ ] Periodic reload of the set from the database
- [ ] Short term deduplication cache
- [ ] [`CANDIDATE` creation with aggressive backoff](/architecture/lifecycle/#backoff-curves)
- [ ] Single host verification run, which is what makes the first check cheap
- [ ] [Wildcard certificate detection](/architecture/discovery/#75-certificate-transparency) into a program flag
- [ ] Per program CT coverage metric

### Milestone 8

- [ ] A certificate issued for a tracked apex produces an asset in under 30 s
- [ ] The service absorbs the full CT stream on a single core
- [ ] A SAN seen ten times in a minute creates one asset
- [ ] A candidate that is never reachable ends `ARCHIVED`, not `INACTIVE`
- [ ] A wildcard certificate sets the flag and lowers the coverage score
- [ ] A candidate that goes live is detected within the hour
- [ ] A candidate's first check runs no enumeration and spends no source quota

## Post-v1

- [ ] An MCP server in front of the search API
- [ ] Per organization pivot display overrides, at the first request
- [ ] RBAC, invitations, SSO, only if third parties use the platform
- [ ] [Additional discovery sources](/architecture/discovery/#76-future-sources): reverse WHOIS, ASN, archives, public repositories
- [ ] **Who writes the screenshot**, which brings back object storage and its key structure
- [ ] **An outbound address distinct from the control plane's**, unverifiable without a production host
- [ ] [Billing a render for real](/architecture/deployment/#95-rate-limiting): the service reports the requests it actually sent to the target
- [ ] [Geographic anomaly](/architecture/verification/#88-geo-ip-and-asn-enrichment), which needs a per program distribution and thirty days of learning
- [ ] [Volatility as an input to the render cadence](/architecture/verification/#cadence-of-the-periodic-render), deferred for lack of measurement rather than lack of code: the tiers need weeks of real data, and fixing them on a few hundred assets would produce invented thresholds that later read as measurements
- [ ] [Email notifications](/architecture/notifications/#126-outputs)
- [ ] Batch pipelining of ingestion, if network cost becomes visible

## Project rules

1. **A red milestone blocks the next phase.** Without exception. An assertion may however be **corrected**
   when it demands something outside its own phase's scope: that is a drafting error in the milestone, not a
   relaxation. The correction is documented.
2. **No phase ends without its regression test.** The milestones become the integration suite.
3. **Phases 2 and 3 do not ship separately.** They are split for implementation order, not for release: a
   detector with no enricher cannot tell a dead origin from a live one.
4. **The interface comes after everything it displays.** It is the most rewarding thing to build and the
   most useless on an empty inventory. Certificate Transparency is the one phase that follows it, and the
   reason is in that phase.
5. **Any structural decision not planned here goes through this document first**, never straight into code.
6. **Deferring an implementation is not closing a phase.** Reordering the code is free; closing a milestone
   with a red assertion is not.
7. **An isolation property is demonstrated by making it fail**, never by observing that it holds. Revoking the
   privilege makes the database answer; rereading the code is a statement about the reader.
8. **A test that does not contain the discriminating case measures nothing.** Ordering and reference frame
   errors, escaping before reversing, comparing a UTC date against the database's calendar day, are invisible
   unless the test data has a wildcard in it and crosses midnight. The habit that catches them is checking
   that a test fails without its fix.
