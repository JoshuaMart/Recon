---
title: Scope
description: A persistent, versioned perimeter, re-evaluated on every ingestion, with three states rather than two.
sidebar:
  order: 5
---

## 5.1 The scope belongs to the control plane

A scanner's scope is a per run setting. What is needed here is a **persistent perimeter per program**,
versioned, that changes over time. So the run definition is **generated** from the database, and the
generated form is deliberately narrower than the stored one:

| Scope rule | In the run definition |
|---|---|
| `include` / `apex` | the run's domain. Several apexes mean several runs |
| `exclude` / `fqdn` | `--exclude admin.example.com` |
| `exclude` / `apex` | `--exclude '*.dev.example.com'` |
| `exclude` / `regex` | `--exclude 're:^staging[0-9]*\.'` |
| `include` / `fqdn`, `cidr`, `url_prefix` | nothing. These classify results, they do not steer a run |
| `exclude` / `cidr` | nothing. Exclusions match names, and an address is only known after resolution |

Pushing exclusions down matters because FastRecon applies them **before any network activity touches
a host**. What cannot be pushed down is applied at ingestion instead, which is where the perimeter is
authoritative anyway.

A pattern that matches nothing comes back as a warning in the report, and Recon surfaces it rather
than counting it as noise: a typo in an exclusion means hosts were scanned that should not have been.

## 5.2 Re-evaluated at ingestion

The scope is re-evaluated **on every ingestion**, not only when a run starts. Every incoming asset is
classified `in_scope`, `out_of_scope` or `unknown` against the rules in force at that moment.

Two benefits. When a program changes its scope, the **history is reclassified** without rescanning.
And out of scope results are **kept** instead of thrown away, which is the real differentiator: most
tools filter at the source and lose the information for good.

It also closes the window a long run opens. A report can land minutes or hours after its rules were
generated, and the rules may have moved in between. The run definition records which ones it used, so
the gap is logged, but the classification that sticks is the current one.

## 5.3 Three states, not two

- `in_scope` matches an include rule and no exclude rule. Actively probed.
- `out_of_scope` matches an exclude rule. Stored, never probed.
- `unknown` was reached through lineage but matches no rule, for instance a SAN pointing at a third
  party domain. Stored, displayed, never probed, and a candidate for review.

`unknown` is where acquisitions, affiliated domains and shared infrastructure show up. Those are the
ones worth asking for a scope extension on.

### Scope is evaluated on the host, and derived assets inherit it

A rule names a host. An asset's key does not always look like one: a service is
`api.target.com:443/tcp` and a URL is `https://api.target.com/v1`. Matching a rule against the key
would put `api.target.com` in scope and leave every service on it out, which is the same perimeter
described two ways and only one of them acted on.

So the classification reads the asset's **host**, which is a
[promoted column filled at creation](/architecture/data-model/#what-the-key-contains-is-filled-at-creation)
rather than parsed at query time. A service takes the status of its host; a URL takes the status of its
service.

**One rule goes the other way**, and it is why the inheritance is a default rather than a law: a
`url_prefix` rule is more specific than a host, so a URL can be excluded while the service carrying it
stays in scope. A child may be stricter than its parent, never looser.

**What that means when a rule changes.** [Reclassification](#a-rule-that-changes-reclassifies-in-the-same-transaction)
is a single pass over the program's assets, and the inheritance is what makes it complete: bringing a
host in scope brings its services and their URLs with it, and taking it out takes them. Without it, a
newly included host would be probed while the services that are the actual surface stayed frozen, and
nothing would report the gap.

The other half is the scheduling, which follows the same pass: an asset becoming `in_scope` gets its
[due dates](/architecture/lifecycle/#63-scheduling-and-backoff), one leaving loses them, and both happen
in the transaction that writes the rule.

## 5.4 Program ownership and expiry

A `program` belongs to exactly one organization. Two organizations tracking the same public target
hold **two independent copies**, with disjoint inventories. That is deliberate: authorization to scan
is granted per organization, scope rules can legitimately diverge, and a shared inventory between
tenants would be an information leak.

`program.platform_ref`, for example `hackerone:united`, is **descriptive**. It is never a join key
between organizations.

**Authorization expiry.** Three states on `program`:

| State | Runs | Data | Visible in search |
|---|---|---|---|
| `active` | yes | live | yes |
| `suspended` | no | frozen, `last_seen` held | yes, with a banner |
| `archived` | no | frozen | on explicit request |

The transition `active` to `suspended` happens automatically when `authorized_to` passes, with a
warning seven days ahead. The scheduler clears the program's due dates, and reports from runs already
in flight are **rejected at ingestion**: a run that started before expiry must not write after it.

The assets of a suspended program **do not change [`lifecycle`](/architecture/lifecycle/#62-state-machine)**.
They did not become inactive; we stopped observing them. Confusing the two would corrupt the
disappearance metrics, which is why suspension is a property of the program and not of its assets.

## 5.5 Managing the perimeter

### A rule that changes reclassifies in the same transaction

A rule in force whose consequence the inventory does not yet carry is a perimeter that lies. One way,
an asset that should be in stays marked out and is not probed, which costs coverage. The other way, an
asset that should be out keeps its due date and **keeps being scanned**, which is a scan outside
authorization, something [11.3](/architecture/security/#113-other-guardrails) treats as first class.

Writing the rule and then reclassifying in two transactions leaves a window where the system scans
what was just taken away from it. The [re-evaluation at ingestion](#52-re-evaluated-at-ingestion)
closes it, but **after** the probe. One probe too many is not much, and it is not a property to accept
knowingly when atomicity is within reach. So the rule write and the program reclassification commit
together.

The price, named rather than discovered: that pass examines every asset of the program and updates the
ones that move. On a program of several hundred thousand assets, a rule change is a long query and a
long transaction. Two things bound it. The scope is the **program**, not the tenant, and the report
returned says what moved, so the effect is immediately readable. The day a program reaches that size,
it is this pass that gets batched, not the atomicity that gets undone.

### A rule is not deleted, it is closed

`scope_rule` carries `valid_from` and `valid_to`, because a rule has a period of validity rather than
an existence. Removing a rule means **setting its `valid_to`**, never a `DELETE`. What that preserves
is what gives [5.3](#53-three-states-not-two) its meaning: an asset classified out of scope by a rule
that has since been closed stays reclassifiable, and one can still explain why it was classified that
way. A deleted rule takes the answer with it.

A program is not deleted either. Its three states are enough, and `archived` is the removal.

### The optimistic lock

`program.version` and `scope_rule.version` exist for one reason: two concurrent writes that silently
lose one another are a lost scope, therefore a scan outside the perimeter. Every modification carries
the version it read, and a stale version is a **409** rather than a write.

What matters is the meaning of the refusal. The client did not make a syntax error. It based a
decision on a state that no longer exists, and the only honest answer is to say so, so it can reread
before rewriting. Creation carries no version, since there is nothing to avoid overwriting.

### What the console can read

The list of programs, one program, and its rules. Nothing more: the assets of a program are
[search](/architecture/search/#101-three-principles) with a filter, and a second path to count them
would be a second set of rules to keep in step.

An asset count still accompanies the list, because that is the question one asks looking at that
screen. It is aggregated on `asset_current`, like a facet, and it is worth saying that this is the only
place in this chapter that costs a scan per program. **These counters are asked for, not given.** The
program switcher sits on every page, so the default shape of the list is the one that costs nothing.

:::note[One clock decides, and it is the one that wrote the value]
Counting the rules in force compares `valid_to` against the current time. `valid_to` is written by the
application and `now()` is PostgreSQL's clock, so the result would depend on two clocks agreeing, and
a rule closed a moment ago would read as still in force for the width of the gap. On a container that
gap is tens of milliseconds, which is enough to make the answer random.

The instant is therefore a parameter, supplied by the clock that wrote the values being compared. A
date comparison never mixes the application's clock with the database's. The fault raises no error,
returns a plausible result, and only shows up on recent values, which are the ones being looked at.
:::
