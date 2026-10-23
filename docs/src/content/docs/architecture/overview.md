---
title: Overview
description: The components, and the direction of every edge between them.
sidebar:
  order: 3
---

```mermaid
flowchart LR
  SE[ScopesExtractor] -->|"GET /, polled"| SY
  CT[certstream] -->|websocket| CM

  subgraph R[Recon, one binary]
    direction TB
    API[HTTP API + UI]
    SY[Scope sync]
    CM[CT matcher]
    SCH[Run scheduler]
    ING[Ingestion]
    RD[Render dispatcher]
    NOT[Notifier]
  end

  R <--> DB[(PostgreSQL)]
  RD --> ST[(Screenshot storage<br/>disk or S3)]

  SCH -->|start run| FR[FastRecon]
  FR -->|GET targets| ING
  FR -->|POST report| ING
  RD -->|POST /scan| FP[Fingerprinter]
  NOT -->|webhook| OUT[Discord or<br/>generic webhook]
  U((You)) --> API
```

| Edge | Meaning |
|---|---|
| ScopesExtractor → scope sync | Recon pulls the scopes and reconciles its programs ([programs](/architecture/programs/)) |
| certstream → CT matcher | new names under a known wildcard become candidates ([discovery](/architecture/discovery/#certificate-transparency)) |
| Run scheduler → FastRecon | starts a run through a runner. FastRecon never touches the database |
| FastRecon → ingestion | fetches its target list, posts one report back |
| Render dispatcher → Fingerprinter | renders due services and ingests the result. The Fingerprinter holds no credential |
| Notifier → webhook | the only outbound alerts |

Loops inside the binary never call each other: they communicate through the database. A report
ingested makes services due for a render by writing their `next_render_at`, and the render dispatcher
picks them up on its next pass.

## One process

API, embedded UI, scope sync, CT matcher, run scheduler, ingestion, render dispatcher and notifier are
one binary. The scanners stay separate services because they already are, and because the
Fingerprinter runs attacker controlled JavaScript and must stay on its own network.

## A run, end to end

```mermaid
sequenceDiagram
  participant S as Run scheduler
  participant F as FastRecon
  participant I as Ingestion
  participant D as PostgreSQL
  S->>D: insert run (queued → starting)
  S->>F: start(definition, tokens)
  F->>I: GET /runs/{id}/targets
  I-->>F: one host per line
  F->>I: POST /runs/{id}/report
  I->>D: observations, lifecycle, changes, due dates (one transaction)
  I->>D: run succeeded
```

Run states and timeouts are in [stack](/architecture/stack/#run-states).
