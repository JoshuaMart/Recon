---
title: Overview
description: The components, and the direction of every edge between them.
sidebar:
  order: 3
---

```
  Certificate             ┌────────────────────────────────────────────────┐
  Transparency ──────────▶│                 CONTROL PLANE                  │
  (certstream)            │  ┌───────────┐  ┌───────────┐  ┌────────────┐  │
       ▲                  │  │ Ingestion │  │ Scheduler │  │  Notifier  │──┼──▶ webhook
       └──────────────────┼──┤    API    │  │           │  │   (diff)   │  │
     apex set (reload)    │  └─────┬─────┘  └─────┬─────┘  └─────┬──────┘  │
                          │        │              │              │         │
                          │  ┌─────▼──────────────▼──────────────▼──────┐  │
  Console ◀───────────────┼─▶│                PostgreSQL                │  │
  (SvelteKit)             │  │  assets · observations · scope · runs    │  │
                          │  └──────────────────────────────────────────┘  │
                          └────┬──────────────────────────────┬────────────┘
                               │                              │
      run definition ▼         │ ▲ report                     │ ▼ POST /scan
      targets URL    ▼         │ │ (webhook)                  │ ▲ result
              ┌──────────────┬─┴─┴──┐              ┌──────────┴─────────────┐
              │         FASTRECON   │              │      FINGERPRINTER     │
              │      (one run, one  │              │     (long-running)     │
              │        report)      │              │                        │
              │  enumerate exclude  │              │  Chrome pool over CDP  │
              │  resolve   portscan │              │  isolated network      │
              │  http probe         │              │  holds no credential   │
              └─────────────────────┘              └────────────────────────┘
```

The direction of each edge is itself a decision.

| Edge | Meaning |
|---|---|
| Scheduler → FastRecon | starts a run and hands it a run definition, never a database |
| FastRecon → Ingestion API | posts one report. This is the main flow of the system |
| Ingestion API → certstream | reloads the apex set when the scope changes ([7.4](/architecture/discovery/#75-certificate-transparency)) |
| Console ↔ Control plane | search, scope edits and manual run requests. The console drives no scanner |
| Control plane → Fingerprinter | `POST /scan` on the five triggers of [8.3](/architecture/verification/#83-when-a-render-happens) |
| Notifier → webhook | the alerts leave here, and only here |

Two things the drawing cannot carry.

**FastRecon never reaches PostgreSQL.** It fetches its target list from a signed URL that expires in
minutes, and posts one report back. That is the whole of its access to the system
([9.1](/architecture/deployment/#91-the-run-contract)).

**The Fingerprinter holds no credential at all.** It is called, it renders, it answers. It has no
route to the database and no route to the internal API, which is what makes running attacker
controlled JavaScript next to an inventory acceptable
([8.5](/architecture/verification/#85-network-isolation)).

Detail per block: [data model](/architecture/data-model/), [discovery](/architecture/discovery/),
[verification](/architecture/verification/), [runs and deployment](/architecture/deployment/).
