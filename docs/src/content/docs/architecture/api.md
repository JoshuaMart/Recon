---
title: HTTP API
description: The routes the UI and scripts call, the two routes a run calls, and how each is authenticated.
sidebar:
  order: 12
---

```mermaid
flowchart LR
  UI[UI in the browser] -->|session cookie| A["/api/*"]
  SC[your scripts] -->|Authorization: Bearer key| A
  FR[FastRecon run] -->|run token| R["/runs/{id}/*"]
```

## Authentication

| Caller | Credential | Reaches |
|---|---|---|
| UI | `POST /api/session` with the API key sets an httpOnly, `Secure`, `SameSite=Strict` signed cookie (30 days) | `/api/*` |
| Scripts | `Authorization: Bearer <api key>` | `/api/*` |
| A FastRecon run | the run's [HMAC tokens](/architecture/stack/#the-run-contract), in a header | its own `/runs/{id}/targets` and `/runs/{id}/report` |

The API key is set in the config. A write with the cookie must also carry an `Origin` matching
`public_url`, which with `SameSite=Strict` covers CSRF.

## Routes

| Route | Does |
|---|---|
| `POST /api/session`, `DELETE /api/session` | log in, log out |
| `GET /api/search?q=&view=services\|hosts&cursor=` | search results |
| `GET /api/search/facets?q=` | facet counts for a query |
| `GET /api/search/fields` | the fields, types and operators the language accepts |
| `GET /api/search/export?q=&format=csv\|json\|urls\|hosts` | export a result |
| `GET /api/assets/{id}` | one asset: current state, technologies, pivots, lineage |
| `GET /api/assets/{id}/changes` | its timeline |
| `GET /api/assets/{id}/screenshots` | its screenshots, served from storage |
| `POST /api/assets/{id}/render`, `POST /api/assets/{id}/probe` | render now, probe now (high priority) |
| `PATCH /api/assets/{id}/triage` | triage state, tags, note, mark as seen |
| `GET /api/changes?q=&cursor=` | the changes feed |
| `GET /api/programs`, `POST /api/programs` | list, create a manual program |
| `GET /api/programs/{id}`, `PATCH /api/programs/{id}` | one program, edit state or auto-scan |
| `POST /api/programs/{id}/rules` | open a local rule |
| `PATCH /api/programs/{id}/rules/{rule}` | close or edit a rule |
| `POST /api/programs/{id}/assets` | enter hosts and URLs |
| `POST /api/programs/{id}/imports/bbot` | import a BBOT `output.json` |
| `POST /api/sync` | run a ScopesExtractor sync now |
| `GET /api/wildcards?sort=oldest` | recon coverage |
| `POST /api/wildcards/{id}/recon` | queue a recon on one wildcard |
| `POST /api/wildcards/recon` | queue a recon on the N oldest, optionally per program |
| `GET /api/runs`, `GET /api/runs/{id}` | runs, with per source accounting |
| `GET`, `POST`, `PATCH`, `DELETE /api/saved-searches` | saved searches |
| `GET /api/health` | sync, CT, Fingerprinter, runners, render queue, storage |
| `GET /runs/{id}/targets` | one host per line, for that run only |
| `POST /runs/{id}/report` | the FastRecon report |

There is no `DELETE` on programs, rules or assets ([P2](/architecture/principles/#p2-nothing-is-deleted)).
Saved searches are your settings, not inventory, so they can be deleted.

## Answers

| Case | Answer |
|---|---|
| unknown search field, bad operator for a type | `400`, `{"error": "unknown_field", "detail": "…", "suggest": "tech"}` |
| missing or wrong credential | `401` |
| unknown id | `404` |
| a report for a run already finished or timed out | `409`, the report is not ingested |
| render or recon asked on an asset or wildcard out of scope | `200` with `queued: false` and a reason |

Lists are paginated with an opaque cursor ordered on a stable key, never with an offset.
