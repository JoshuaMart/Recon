---
title: Roadmap
description: The build order, each phase ending in something usable.
sidebar:
  order: 14
---

Each phase ends with something you can use, and with integration tests for what it added. A phase
starts once the previous one works end to end. ✓ marks a finished phase.

```mermaid
flowchart LR
  P0[0 Foundations] --> P1[1 Programs and sync]
  P1 --> P2[2 Recon and ingestion]
  P2 --> P3[3 Verification and lifecycle]
  P3 --> P4[4 Fingerprinting]
  P4 --> P5[5 Search language]
  P5 --> P6[6 Interface]
  P6 --> P7[7 Changes and notifications]
  P7 --> P8[8 Certificate Transparency]
  P8 --> P9[9 Scaleway runner]
  P9 --> P10[10 BBOT import]
```

| Phase | Delivers | Done when |
|---|---|---|
| **0 Foundations** ✓ | repo, compose stack, config, migrations, CI, backup and restore | `make up` starts everything, a restore works, isolation is checked |
| **1 Programs and sync** ✓ | programs, rules, wildcards, ScopesExtractor sync, manual programs, API authentication and the program routes | a real ScopesExtractor instance fills programs, a removed scope closes its rule |
| **2 Recon and ingestion** ✓ | assets and scope classification, assets declared by rules and by hand, the run queue and the `serve` runner, report ingestion, normalization, dedup, changes, auto-scan | a recon on a real wildcard fills the inventory, a second run writes almost no new observations |
| **3 Verification and lifecycle** ✓ | due dates, verification runs, qualification, state machine, backoff | a host taken down goes `flapping` then `inactive`, a timeout never does |
| **4 Fingerprinting** ✓ | render triggers, `asset_tech`, versions, screenshots and storage | every new service gets a render, screenshot sizes measured |
| **5 Search language** ✓ | parser, compiler, fields, facets, pivots, export | every example of the search page returns what it says |
| **6 Interface** ✓ | the screens, triage, wildcards view | a full hunt session without touching the API |
| **7 Changes and notifications** ✓ | change rows, feed, saved search alerts, Discord | a week of use sends only what the defaults list |
| **8 Certificate Transparency** ✓ | CT matcher, candidate flow, the certificate notification | a certificate for a new name under a wildcard is probed within a minute |
| **9 Scaleway runner** ✓ | the [`scaleway` runner](/architecture/stack/#runs): FastRecon in a Serverless Job, `GET /runs/{id}/targets` and `POST /runs/{id}/report` behind HMAC tokens, the runner chosen per run kind in the config | a recon and a verification run in a job, scanning from an address other than the VPS, and their reports are ingested like a `serve` run's |
| **10 BBOT import** | the [BBOT import](/architecture/discovery/#bbot-import): parser, the import route and `recon import bbot` | a BBOT `output.json` declares its names, classified against the program's scope, and observes nothing |

## Later

- Scheduled recon of the oldest wildcards, if on-demand turns out not to be enough
- More discovery sources: reverse WHOIS, ASN, archives
- An MCP server in front of the search API
- Screenshot diffing shown in the timeline

## Rules

1. A structural decision not written here goes through this site first, then into code.
2. A test that does not contain the discriminating case measures nothing: check it fails without the fix.
3. The interface comes after what it displays.
