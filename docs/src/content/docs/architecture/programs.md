---
title: Programs and scope
description: Programs come from ScopesExtractor or are entered by hand. The scope is a set of rules, and wildcards are first-class.
sidebar:
  order: 4
---

## Two kinds of program

| Origin | Created by | Scope edited by | Example |
|---|---|---|---|
| `scopesextractor` | the sync | the sync, plus local rules | a HackerOne program |
| `manual` | you | you | MSRC, a private contract |

A synced program is identified by `(platform, slug)`. A manual program has no platform identity and the
sync never touches it.

## The sync

Recon polls ScopesExtractor's `GET /` every hour (configurable; ScopesExtractor itself syncs the
platforms every three hours) and reconciles against the full state. A pass that fails changes nothing,
and the next one repairs it.

Each entry carries `platform`, `slug`, `program_name`, `bounty`, `value`, `type` and `is_in_scope`.
Recon groups entries by `(platform, slug)` into programs.

```mermaid
flowchart TD
  P[Poll ScopesExtractor] --> C{Compare with<br/>synced rules}
  C -->|new program| NP[create program]
  C -->|program gone| AP[archive program]
  C -->|new scope| NR[open rule]
  C -->|scope gone| CR[close rule<br/>valid_to = now]
  C -->|unchanged| N[nothing]
  NR --> RC[reclassify assets]
  CR --> RC
  NR -->|wildcard| W[new wildcard<br/>auto-scan?]
```

A changed scope is a closed rule plus an opened one. ScopesExtractor already notifies scope changes on
Discord, so Recon does not repeat them.

### How a scope value becomes a rule

| ScopesExtractor value | Rule | Side effect |
|---|---|---|
| `*.example.com` | `apex example.com` | a [wildcard](#wildcards) |
| `api.example.com` | `fqdn api.example.com` | the host is declared as an asset |
| `https://example.com/app/` | `url_prefix` | the URL, its service and its host are declared |
| `10.0.0.0/24`, `10.0.0.1` | `cidr` (a single address is a `/32`) | none, it classifies resolved addresses |
| mobile app, executable, other | not stored as a rule | listed on the program as untracked |
| any entry with `is_in_scope: false` | the same matcher, as an `exclude` rule | |

Platforms rarely expose their out of scope list through ScopesExtractor, so most exclusions are
[local rules](#local-rules).

### Local rules

A synced program can carry **local** rules, typically an exclusion or an extra host. The sync owns
only the rules it created, so it never closes a local one.

## Scope evaluation

Every asset is classified each time it is written, and every time a rule changes.

```mermaid
flowchart LR
  A[asset host] --> X{matches an<br/>exclude rule?}
  X -->|yes| O[out_of_scope]
  X -->|no| I{matches an<br/>include rule?}
  I -->|yes| IN[in_scope]
  I -->|no| U[unknown]
```

| Status | Stored | Probed | Typical case |
|---|---|---|---|
| `in_scope` | yes | yes | under a wildcard |
| `out_of_scope` | yes | no | excluded host |
| `unknown` | yes | no | a third party name reached through a CNAME or a SAN |

A service or a URL takes the status of its host, except that a `url_prefix` exclusion can take one path
out while its service stays in.

## Program states

```mermaid
stateDiagram-v2
  [*] --> active
  active --> paused: you pause it
  paused --> active
  active --> archived: gone from the platform, or you archive it
  paused --> archived
  archived --> active: back on the platform
```

A paused or archived program is never probed. Its assets keep their lifecycle: we stopped looking, the
targets did not die.

## Wildcards

Every `apex` rule is a wildcard, and a wildcard carries its own recon history.

| Column | Meaning |
|---|---|
| `last_recon_at` | end of the last discovery run on it, `NULL` if never |
| `last_recon_run` | that run |
| `assets_found` | assets whose lineage starts at this wildcard |
| `auto_scan` | inherited from the program, then the global setting |

### Auto-scan

| Setting | When a new wildcard appears |
|---|---|
| `off` | nothing, it shows up as never scanned |
| `on_new` | one discovery run is queued |

Global default in the config, overridable per program. There is no per wildcard setting: the
[wildcards view](/architecture/interface/#wildcards) shows the value in effect.

A queued recon is a `run` row in state `queued`. Runs start oldest wildcard first under a concurrency
cap ([run states](/architecture/stack/#run-states)), so a first sync bringing two hundred wildcards does
not start two hundred scans at once.
