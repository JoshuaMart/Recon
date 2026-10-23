---
title: Interface
description: The screens of the console, and what each one is for.
sidebar:
  order: 11
---

A static single page app, served by the Recon binary. Access is one API key, entered once and kept in
an httpOnly cookie ([API](/architecture/api/#authentication)).

```mermaid
flowchart LR
  S[Search] --> A[Asset]
  F[Changes feed] --> A
  P[Programs] --> PR[Program] --> W[Wildcards]
  PR --> S
  W --> R[Runs]
  A --> S
```

## Screens

| Screen | For |
|---|---|
| **Search** | the main screen: query box, facets, results with screenshots, saved searches |
| **Asset** | everything about one asset, and its history |
| **Changes** | the feed of what moved, unseen first |
| **Programs** | the list, origin, state, counts, sync status |
| **Program** | scope rules, local rules, wildcards, untracked scopes, settings |
| **Wildcards** | recon coverage across every program |
| **Runs** | running and past runs, per source accounting, errors |
| **Settings** | saved searches, config in effect (read-only), storage usage, health |

## Search

```
┌─────────────────────────────────────────────────────────────────────┐
│ tech:nginx@<1.25 status:2xx -cdn:*                  [Save] [Export] │
├──────────────┬──────────────────────────────────────────────────────┤
│ Technologies │ ┌──────┐ app.acme.com:443   200  "Dashboard"        │
│  nginx   412 │ │ shot │ nginx 1.22.1 · PHP 8.1 · jQuery 3.5         │
│  PHP     201 │ └──────┘ acme · first seen 3d · new                  │
│ Status       │ ┌──────┐ dev.foo.io:8443    200  "Grafana"          │
│  2xx     389 │ │ shot │ nginx 1.18.0 · Grafana 9.1                  │
│ Ports        │ └──────┘ foo · first seen 40d · seen                 │
│  443     300 │                                                      │
└──────────────┴──────────────────────────────────────────────────────┘
```

Results can be shown as a list or as a **screenshot grid**, the fastest way to scan a large result by eye.

## Asset

| Block | Contents |
|---|---|
| Header | key, program, scope, lifecycle, triage state, tags, notes |
| Screenshot | latest, with older distinct ones |
| Current state | DNS, ports, HTTP, redirect chain, technologies with versions, headers, cookies |
| Pivots | favicon, scripts, cookies, each with how many other assets share it |
| Timeline | changes over time, newest first |
| Lineage | why it is here |
| Actions | render now, probe now, triage |

## Triage

Each asset carries your own state, independent of its lifecycle.

| State | Meaning |
|---|---|
| `new` | never opened |
| `seen` | opened |
| `interesting` | worth coming back to |
| `ignored` | hidden from default views |

Plus free tags and a note. All of it is searchable (`triage:interesting tag:idor`).

## Wildcards

The answer to "what have I not scanned in a while".

| Wildcard | Program | Last recon | Assets | Auto-scan |
|---|---|---|---|---|
| `*.example.com` | Example | never | — | off |
| `*.foo.com` | Foo | 94 days ago | 1 204 | on_new |
| `*.acme.com` | Acme | 3 days ago | 3 310 | on_new |

Auto-scan shows the value in effect, from the program or the global default. Sorted oldest first, never
scanned on top. Actions: **scan this one**, **scan the N oldest**, filter by
program or platform.

## Copy

Interface in English, sentence case, no em dashes.
