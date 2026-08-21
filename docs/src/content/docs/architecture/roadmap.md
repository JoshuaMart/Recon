---
title: Roadmap
description: Nine phases in a constrained order, each closed by a milestone made of testable assertions.
sidebar:
  order: 15
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

- [ ] Repository initialized, [structure decided](/architecture/stack/#132-repository-structure)
- [ ] [Migration tool wired](/architecture/stack/#134-migrations-goose), versioned and reversible
- [ ] Local Docker Compose: [PostgreSQL](/architecture/stack/#133-self-hosted-postgresql), the Fingerprinter and its Chrome sidecar, on [the two networks](/architecture/verification/#85-network-isolation). FastRecon is not a service: it is an image a run starts
- [ ] [Minimal CI](/architecture/stack/#138-tooling-and-ci): lint, tests, build
- [ ] [Typed configuration](/architecture/stack/#136-configuration-and-secrets), no hard coded value
- [ ] [Secrets out of the repository](/architecture/stack/#136-configuration-and-secrets), injected from the environment
- [ ] [Database backup](/architecture/stack/#backups-the-consequence-of-self-hosting), archiving to a destination that is configuration, so the restore is exercised locally
- [ ] [Two PostgreSQL roles](/architecture/deployment/#96-postgresql-roles), before any business migration

### Milestone 0

- [ ] `docker compose up` starts everything in under 60 s on a clean machine
- [ ] A migration can be applied then rolled back without loss
- [ ] CI passes on an empty pull request
- [ ] A backup is restored into an empty database from the configured archive destination, and the content is identical
- [ ] Connected as `asm_app`: `CREATE TABLE` fails, `DROP TABLE` fails on a table the owner created, and reading and writing that table succeeds through the default privileges
- [ ] From the fingerprinter's container, `psql` to the database fails, and the same connection succeeds from the control plane network

## Phase 1: Data model and ingestion

**Goal**: freeze the [model](/architecture/data-model/). This is the most irreversible phase of the project.

- [ ] `org`, `app_user`, `membership`, multi-tenant from the first migration
- [ ] `program` and `scope_rule` with their validity windows and their `version`
- [ ] `asset` with [canonical keys](/architecture/data-model/#43-canonical-keys), lineage and scope status
- [ ] `observation`, **[partitioned monthly](/architecture/data-model/#partitioning-from-the-first-migration)** from the start
- [ ] `asset_current` and `asset_layer`
- [ ] `run` and `run_target`
- [ ] `pivot_count`, `favicon_image`, and the seeded denylist with its idempotent seed
- [ ] `org_id` on **every** business table
- [ ] Indexes: GIN on JSONB and arrays, B-tree on promoted columns, the `reverse(key)` expression index
- [ ] The single [`normalize(layer, data)`](/architecture/data-model/#normalization-comes-first) function, with its per layer schemas
- [ ] `POST /reports` with [scope re-evaluation](/architecture/scope/#52-re-evaluated-at-ingestion) and [deduplication on write](/architecture/data-model/#deduplication-on-write)
- [ ] [Geo-IP and ASN enrichment](/architecture/verification/#88-geo-ip-and-asn-enrichment) in memory in the control plane
- [ ] [One authorization layer](/architecture/security/#111-irreversible-decisions) receiving a principal

### Milestone 1

- [ ] Posting the same report twice creates **one** series of observations
- [ ] Posting 1 000 identical observations creates **one** row, with `last_confirmed_at` moved
- [ ] The measured deduplication rate exceeds 90 % on a replayed set
- [ ] An out of scope asset is stored, never deleted, and marked `out_of_scope`
- [ ] Changing a `scope_rule` reclassifies history without rescanning
- [ ] A query carrying a different `org_id` returns **no** row
- [ ] A report attached to an expired `program` is rejected at ingestion
- [ ] A report naming a host outside its run's frozen target list is **rejected**, not ignored
- [ ] Monthly partitions are created automatically, and a row outside any partition fails
- [ ] Connected as `asm_app`: `DROP TABLE asset` fails, writing to the seeded denylist fails, reading it succeeds, and `INSERT INTO asset` succeeds
- [ ] The ingestion cost stays under the round trip budget per observation, measured rather than estimated

## Phase 2: Verification and lifecycle

**Goal**: liveness on a direct target. Usable immediately on a hand entered list, which is what makes this
phase testable before discovery exists.

- [ ] [Run definitions](/architecture/deployment/#91-the-run-contract), signed target URL, signed report token
- [ ] [Due date selection](/architecture/lifecycle/#63-scheduling-and-backoff) and the frozen target list as the lease
- [ ] Deadline sweeper for runs, and the console refusal that names the run
- [ ] [Backoff with jitter](/architecture/lifecycle/#backoff-curves), two curves
- [ ] [Failure qualification](/architecture/lifecycle/#where-the-qualification-is-carried), informative against non informative
- [ ] [The `lifecycle` state machine](/architecture/lifecycle/#62-state-machine), excluding `unobservable`, which depends on phase 3
- [ ] [Projection onto `asset_current`](/architecture/lifecycle/#three-layers-one-lifecycle): layer states, promoted columns, reachability counters
- [ ] [An open port becomes an asset](/architecture/verification/#an-open-port-becomes-an-asset), with its bound
- [ ] [Dangling CNAME detection](/architecture/verification/#takeover-candidates) into a structured field
- [ ] [Dead origin signatures behind a CDN](/architecture/verification/#dead-origin-behind-a-cdn) and `is_cdn` detection
- [ ] Manual asset entry, under a scope action rather than the ingestion one

### Milestone 2

- [ ] A hand entered list of 100 FQDNs produces a correct inventory
- [ ] An `nxdomain` confirmed 3 times over more than 24 h moves the asset to `INACTIVE`
- [ ] An `nxdomain` confirmed 3 times in 90 minutes does **not**
- [ ] A repeated timeout **never** produces an `INACTIVE`
- [ ] An asset that comes back after a failure returns to `ACTIVE` on a single success
- [ ] A host absent from a truncated report gets no observation and keeps its due date
- [ ] A run killed mid flight blocks nothing: its targets are selected again on the next tick
- [ ] Two concurrent runs never hold the same host
- [ ] A port found open becomes a `service` asset, and 25 open ports on one host derive nothing
- [ ] A dangling CNAME is recorded with `kind`, target, signature and timestamp, which is enough for phase 5 without recollecting anything

## Phase 3: Fingerprinting

**Goal**: [baseline, reachability, and death behind a CDN](/architecture/verification/#82-the-fingerprinter).
Inseparable from phase 2 in use.

- [ ] The service deployed on an [isolated network](/architecture/verification/#85-network-isolation), two Docker networks locally
- [ ] No route to PostgreSQL and none to the internal API, **verified rather than assumed**, with a positive control
- [ ] The service holds **no credential**
- [ ] SSRF guards: internal ranges refused before the request and at every redirect hop
- [ ] `POST /scan` from the control plane, with a render costing more than one against the budget
- [ ] `high` and `low` queues, a baseline entering low
- [ ] [The five triggers](/architecture/verification/#83-when-a-render-happens) and the baseline filter
- [ ] [Reachability per observer](/architecture/verification/#86-reachability-per-observer), signed counters, the four regimes
- [ ] The [`unobservable`](/architecture/lifecycle/#64-qualifying-a-failure) state and the per program threshold alert
- [ ] [`producer_version` and `last_producer_version`](/architecture/verification/#87-dating-the-instrument)
- [ ] [Periodic cadence modulated by volatility](/architecture/verification/#cadence-of-the-periodic-render)

### Milestone 3

- [ ] From the service's container, `psql` to the database **fails**, and the same connection **succeeds** from the control plane network
- [ ] A request toward `169.254.169.254` is refused, and so is a redirect hop toward an internal range
- [ ] An asset whose two probes fail 3 times becomes `unobservable`, **not** `INACTIVE`
- [ ] An asset at 403 on the HTTP layer and 200 on the render flips to the protected regime
- [ ] A dead origin behind a CDN is detected as dead, including when the render fails at the same moment
- [ ] No screenshot is present in PostgreSQL
- [ ] A render that produces no chain writes an observation, and the asset's counters move
- [ ] `last_fingerprint_at` does not move on a render that obtained no page
- [ ] A pass over 500 assets does not exceed the program's rate limit

## Phase 4: Discovery

**Goal**: [FastRecon over a program's apexes](/architecture/discovery/), feeding the loop of phase 2.

- [ ] [Run definition generated from `scope_rule`](/architecture/scope/#51-the-scope-belongs-to-the-control-plane)
- [ ] [Source credentials](/architecture/discovery/#73-source-credentials) in the run's environment, an empty variable refusing startup
- [ ] [The curated port list](/architecture/discovery/#74-the-port-list) travelling in the run definition
- [ ] [Scheduled runs](/architecture/deployment/#98-starting-runs) per program and the console endpoint
- [ ] Assets entering through the normal path: ingestion, scope, due dates
- [ ] [Per host source attribution](/architecture/data-model/#44-lineage) feeding lineage

### Milestone 4

- [ ] A run killed midway keeps everything already delivered
- [ ] No out of scope asset receives a due date
- [ ] Rescanning the same perimeter twice creates **no** observation row on unchanged assets
- [ ] A run that never completes is marked expired and a replacement is provisioned
- [ ] [`discovery_path`](/architecture/data-model/#44-lineage) is populated and usable on every asset
- [ ] A source without a key reports itself, and the run says in one line what it could query

## Phase 5: Diff and notifications

**Goal**: [the system becomes better than a script](/architecture/notifications/).

- [ ] [`notification_event`](/architecture/notifications/#123-where-the-event-is-produced) written **inside the ingestion transaction**, payload frozen, with its `CHECK`
- [ ] Monthly partitioning, and a maintenance loop that starts unconditionally, for **both** partitioned tables
- [ ] [Asymmetric retention](/architecture/notifications/#127-volume-and-retention)
- [ ] The write folded into the statements that already exist
- [ ] [Structured diffs](/architecture/notifications/#121-structured-diffs) per field type
- [ ] [Revelation against real change](/architecture/verification/#87-dating-the-instrument) classification
- [ ] The [event table](/architecture/notifications/#122-events-worth-notifying), excluding `geo_anomaly` and `external_host_dead`
- [ ] Notifier loop, and the two program events it alone can produce
- [ ] [Anti-flood](/architecture/notifications/#124-aggregation-and-anti-flood) and the [first run grace](/architecture/notifications/#125-the-first-run-of-a-program) with its age guardrail
- [ ] [Per organization channels](/architecture/notifications/#channels-belong-to-an-organization) and a generic templated webhook

### Milestone 5

- [ ] An application version bump produces a readable notification saying *what* changed
- [ ] A rescan with no real change produces **no** notification
- [ ] On a **synthetic** set of 5 000 assets, a first run produces **one** summary and 5 000 suppressed events
- [ ] On the same set, a later run turning 5 000 assets active sends at most 20 notifications per window, the rest carried by summaries, and **no event lost**
- [ ] A takeover candidate is notified at critical priority, at most one Notifier tick after the event was written
- [ ] A fingerprinter version bump produces diffs classified as detection improved, with no alert
- [ ] A dangling CNAME recorded in phase 2 produces a critical notification **without recollecting anything**
- [ ] A mass tip into `unobservable` triggers a distinct alert rather than being swallowed by a summary
- [ ] A webhook answering 500 marks **no** event notified, and sending resumes on recovery
- [ ] A program event with no `asset_id` is accepted, and a `takeover_candidate` without one is **refused by the database**
- [ ] A failed first run leaves the grace active; a later successful one ends it
- [ ] A grace nothing ends expires at 7 days, emits its summary and reports the incident
- [ ] The ingestion cost stays under budget, events included, on a batch where everything changes

## Phase 6: Search and facets

- [ ] [Pivot promotion into `asset_current`](/architecture/search/#102-what-the-projection-carries), the precondition for everything else
- [ ] [Structured AST to parameterized SQL](/architecture/search/#101-three-principles), with a field registry
- [ ] [Facets over the filtered result](/architecture/search/#104-facets-are-the-real-cost)
- [ ] [`pivot_count` maintained on write](/architecture/search/#105-pivots): increment, value loss **and** archiving
- [ ] [Genericity filter](/architecture/search/#the-genericity-filter) applied to display only
- [ ] [`favicon_image`](/architecture/search/#the-favicon-image-is-not-in-attributes) and its size bound
- [ ] [Volatility](/architecture/search/#106-volatility-as-sliding-daily-buckets) as sliding buckets
- [ ] Stable pagination and [export](/architecture/search/#108-export)
- [ ] [`external_host_dead`](/architecture/notifications/#what-external_host_dead-can-actually-see), both halves
- [ ] [The third role and the second pool](/architecture/deployment/#96-postgresql-roles), and [Row-Level Security](/architecture/security/#row-level-security-two-roles-rather-than-one-variable) enabled

### Milestone 6

- [ ] The RLS suite **refuses to run** if `current_user` is a superuser, carries `BYPASSRLS`, or owns the tables
- [ ] An `asm_app` session set to one organization reads **no** row of another, without the query carrying `org_id`
- [ ] A connection acquired **without** the session variable returns zero rows, never the whole inventory
- [ ] The cross-tenant queries cross tenants under `asm_sys` and return only the set organization under `asm_app`
- [ ] A four clause query on a **synthetic** set of 1 M assets, with a realistic distribution, answers in under 300 ms
- [ ] Facets reflect the filtered result, not the global inventory
- [ ] A pivot with a counter of 1 is not displayed, and a universal cookie name never appears as a pivot
- [ ] An asset losing a pivot value decrements its counter; an archived asset gives back **all** of its own and loses the keys
- [ ] A suffix containing a wildcard character still matches by suffix rather than by equality
- [ ] The export applies no display filter and imposes no silent cap
- [ ] The ingestion cost stays under budget, pivots included, measured on a batch that moves some
- [ ] **No query touches `observation`**, proved by revoking the privilege

## Phase 7: Console

- [ ] [`recon bootstrap`](/architecture/deployment/#97-bootstrap), a precondition for the rest of the phase
- [ ] [SvelteKit console](/architecture/console/) with no database credential, and the token exchange
- [ ] [Host grouped list](/architecture/search/#107-the-list-is-a-list-of-hosts) with facets
- [ ] [Distinct timestamp on fingerprinter badges](/architecture/search/#what-a-row-carries)
- [ ] [Three enrichment states](/architecture/search/#three-states-of-enrichment), the console holding that state from the server
- [ ] [Takeover candidate rendered on the row](/architecture/search/#what-a-row-carries)
- [ ] [Asset view with its timeline](/architecture/search/#109-the-asset-view), the project's first read of the journal
- [ ] [Live SSE feed](/architecture/search/#1010-the-live-feed-of-discoveries), polled on a cursor
- [ ] [Program and scope management](/architecture/scope/#55-managing-the-perimeter), reclassifying in the same transaction
- [ ] [Queue view](/architecture/deployment/#99-reading-the-queue)

### Milestone 7

- [ ] Deciding "open it or move on" on a row in under a second
- [ ] No composite score, no severity, no environment label, held by a test on the **contract** rather than on a screen
- [ ] An `unobservable` asset is visually distinct from an `INACTIVE` one
- [ ] Geolocation is not shown on a CDN asset
- [ ] Every pivot badge is clickable and re-runs a search
- [ ] A row with no cookie badge distinguishes the three causes
- [ ] On a deployment with no Geo-IP database, the infrastructure family is **not shown** rather than empty
- [ ] A row carries **no** script hash badge, and the same hash stays searchable and counted
- [ ] `recon bootstrap` creates a usable organization without a line of SQL, and replayed it creates no second one
- [ ] A service discovered and never probed is found by a filter on the port its key carries
- [ ] The live feed re-emits nothing on an unchanged rescan, and says what a cap left out
- [ ] A scope rule write grants or removes due dates in the same transaction, and a failed reclassification writes no rule
- [ ] A write carrying a stale version answers 409 and applies **nothing**
- [ ] The console **refuses to start** without `ORIGIN` outside development; in a production build, a cross-origin POST and a POST with no `Origin` both answer 403 where the same form in same origin answers 200

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
